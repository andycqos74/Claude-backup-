package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"centralbackup/internal/proto"
)

// sqlOpenOld opens a raw sqlite handle (driver registered by the package's
// blank import) for constructing a pre-migration database in tests.
func sqlOpenOld(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path)
}

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUsersAndSessions(t *testing.T) {
	s := openTest(t)
	if n, _ := s.CountUsers(); n != 0 {
		t.Fatal("fresh db has users")
	}
	if err := s.CreateUser("admin", "hunter22"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("admin", "wrong"); err != ErrNotFound {
		t.Fatal("wrong password accepted")
	}
	u, err := s.Authenticate("admin", "hunter22")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateSession(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionUser(token)
	if err != nil || got.Username != "admin" {
		t.Fatalf("SessionUser: %v %v", got, err)
	}
	if err := s.DeleteSession(token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(token); err != ErrNotFound {
		t.Fatal("session survived deletion")
	}
}

func TestEnrollTokenSingleUse(t *testing.T) {
	s := openTest(t)
	token, err := s.CreateEnrollToken("test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeEnrollToken(token, "agent1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeEnrollToken(token, "agent2"); err != ErrNotFound {
		t.Fatal("token reusable")
	}
	// Expired token.
	token2, _ := s.CreateEnrollToken("test", -time.Hour)
	if err := s.ConsumeEnrollToken(token2, "agent3"); err != ErrNotFound {
		t.Fatal("expired token accepted")
	}
}

func TestAgentsAndAuth(t *testing.T) {
	s := openTest(t)
	id, secret, err := s.CreateAgent("box", proto.EnrollRequest{Hostname: "box", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.VerifyAgent(id, secret) {
		t.Fatal("valid credentials rejected")
	}
	if s.VerifyAgent(id, "bad") || s.VerifyAgent("nope", secret) {
		t.Fatal("invalid credentials accepted")
	}
	a, err := s.GetAgent(id)
	if err != nil || a.Hostname != "box" {
		t.Fatalf("GetAgent: %+v %v", a, err)
	}
}

func TestJobsRoundTrip(t *testing.T) {
	s := openTest(t)
	j := proto.Job{
		ID: NewID(), AgentID: "a1", Name: "docs", Paths: []string{"/x"},
		Schedule: "0 2 * * *", Enabled: true, Origin: proto.OriginServer, Version: 1,
	}
	if err := s.SaveJob(j); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetJob(j.ID)
	if err != nil || row.Job.Name != "docs" || row.Job.Version != 1 {
		t.Fatalf("GetJob: %+v %v", row, err)
	}
	j.Version = 2
	j.Name = "docs2"
	if err := s.SaveJob(j); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListJobs("a1")
	if err != nil || len(rows) != 1 || rows[0].Job.Name != "docs2" {
		t.Fatalf("ListJobs: %+v %v", rows, err)
	}
	if err := s.DeleteJob(j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetJob(j.ID); err != ErrNotFound {
		t.Fatal("job survived deletion")
	}
}

func TestRunsAndSnapshots(t *testing.T) {
	s := openTest(t)
	runID, err := s.CreateRun("j1", "a1", proto.ModeFull, proto.RunRunning)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSnapshot(Snapshot{ID: "s1", Backend: "local", JobID: "j1", AgentID: "a1", RunID: runID, Mode: proto.ModeFull, Files: 3, Bytes: 100, ManifestKey: "manifests/s1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(runID, proto.RunSuccess, "s1", "", proto.RunStats{FilesTotal: 3}); err != nil {
		t.Fatal(err)
	}
	run, err := s.GetRun(runID)
	if err != nil || run.Status != proto.RunSuccess || run.SnapshotID != "s1" {
		t.Fatalf("GetRun: %+v %v", run, err)
	}
	latest, err := s.LatestSnapshot("local", "j1")
	if err != nil || latest.ID != "s1" {
		t.Fatalf("LatestSnapshot: %+v %v", latest, err)
	}
	// A different backend has no snapshot for this job yet.
	if _, err := s.LatestSnapshot("onedrive/x/y", "j1"); err != ErrNotFound {
		t.Fatalf("LatestSnapshot on empty backend: want ErrNotFound, got %v", err)
	}

	// Disconnect fails running runs but preserves queued ones.
	r2, _ := s.CreateRun("j1", "a1", proto.ModeIncremental, proto.RunRunning)
	q1, _ := s.CreateRun("j1", "a1", proto.ModeIncremental, proto.RunQueued)
	if err := s.FailRunningRuns("a1", "agent disconnected"); err != nil {
		t.Fatal(err)
	}
	run2, _ := s.GetRun(r2)
	if run2.Status != proto.RunError {
		t.Fatal("running run not failed on disconnect")
	}
	queued, _ := s.QueuedRuns("a1")
	if len(queued) != 1 || queued[0].ID != q1 {
		t.Fatalf("queued runs: %+v", queued)
	}
}

// TestMigrateBackendScoping verifies that an existing pre-scoping database
// (blobs keyed by hash only, snapshots without a backend column) upgrades
// cleanly, with existing rows assigned to the 'local' backend.
func TestMigrateBackendScoping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Build an old-schema DB directly, then let Open() migrate it.
	raw, err := sqlOpenOld(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE blobs (hash TEXT PRIMARY KEY, size_raw INTEGER NOT NULL, size_stored INTEGER NOT NULL, created_at INTEGER NOT NULL);
		INSERT INTO blobs VALUES ('oldhash', 10, 5, 1);
		CREATE TABLE snapshots (id TEXT PRIMARY KEY, job_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL, files INTEGER NOT NULL DEFAULT 0, bytes INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, manifest_key TEXT NOT NULL);
		INSERT INTO snapshots (id, job_id, agent_id, mode, created_at, manifest_key) VALUES ('oldsnap', 'j1', 'a1', 'full', 1, 'manifests/oldsnap');
	`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate) failed: %v", err)
	}
	defer s.Close()

	// The pre-existing blob and snapshot must now belong to 'local'.
	if ok, _ := s.HasBlob("local", "oldhash"); !ok {
		t.Fatal("migrated blob not found under 'local' backend")
	}
	if ok, _ := s.HasBlob("onedrive/x/y", "oldhash"); ok {
		t.Fatal("migrated blob must not appear under another backend")
	}
	snaps, err := s.ListSnapshots("local", "", "j1")
	if err != nil || len(snaps) != 1 || snaps[0].ID != "oldsnap" {
		t.Fatalf("migrated snapshot: %+v %v", snaps, err)
	}
	if snaps[0].Backend != "local" {
		t.Fatalf("migrated snapshot backend = %q, want local", snaps[0].Backend)
	}
}

func TestBlobs(t *testing.T) {
	s := openTest(t)
	if err := s.AddBlob("local", "h1", 10, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBlob("local", "h1", 10, 5); err != nil {
		t.Fatal("duplicate AddBlob must be a no-op, got", err)
	}
	missing, err := s.MissingBlobs("local", []string{"h1", "h2"})
	if err != nil || len(missing) != 1 || missing[0] != "h2" {
		t.Fatalf("MissingBlobs: %v %v", missing, err)
	}
	st, err := s.Stats("local")
	if err != nil || st.Blobs != 1 || st.SizeRaw != 10 || st.SizeStored != 5 {
		t.Fatalf("Stats: %+v %v", st, err)
	}
}

// TestBlobsPerBackend verifies the dedup index is scoped per backend: the
// same hash tracked in one backend is absent (and must be re-uploaded) in
// another. This is the fix for the "switch backend -> blob not found" bug.
func TestBlobsPerBackend(t *testing.T) {
	s := openTest(t)
	if err := s.AddBlob("local", "h1", 10, 5); err != nil {
		t.Fatal(err)
	}
	// Present in local...
	if ok, _ := s.HasBlob("local", "h1"); !ok {
		t.Fatal("h1 should be present in local")
	}
	// ...but not in a cloud backend, so it will be re-uploaded there.
	if ok, _ := s.HasBlob("onedrive/acct/folder", "h1"); ok {
		t.Fatal("h1 must NOT be considered present in a different backend")
	}
	missing, _ := s.MissingBlobs("onedrive/acct/folder", []string{"h1"})
	if len(missing) != 1 || missing[0] != "h1" {
		t.Fatalf("h1 should be missing in the cloud backend, got %v", missing)
	}
	// Same hash can be independently tracked in both backends.
	if err := s.AddBlob("onedrive/acct/folder", "h1", 10, 5); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasBlob("onedrive/acct/folder", "h1"); !ok {
		t.Fatal("h1 should now be present in the cloud backend")
	}
	// Deleting from one backend leaves the other intact.
	if err := s.DeleteBlob("local", "h1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasBlob("local", "h1"); ok {
		t.Fatal("h1 should be gone from local")
	}
	if ok, _ := s.HasBlob("onedrive/acct/folder", "h1"); !ok {
		t.Fatal("h1 should still be present in the cloud backend")
	}
}
