package store

import (
	"path/filepath"
	"testing"
	"time"

	"centralbackup/internal/proto"
)

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
	if err := s.CreateSnapshot(Snapshot{ID: "s1", JobID: "j1", AgentID: "a1", RunID: runID, Mode: proto.ModeFull, Files: 3, Bytes: 100, ManifestKey: "manifests/s1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(runID, proto.RunSuccess, "s1", "", proto.RunStats{FilesTotal: 3}); err != nil {
		t.Fatal(err)
	}
	run, err := s.GetRun(runID)
	if err != nil || run.Status != proto.RunSuccess || run.SnapshotID != "s1" {
		t.Fatalf("GetRun: %+v %v", run, err)
	}
	latest, err := s.LatestSnapshot("j1")
	if err != nil || latest.ID != "s1" {
		t.Fatalf("LatestSnapshot: %+v %v", latest, err)
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

func TestBlobs(t *testing.T) {
	s := openTest(t)
	if err := s.AddBlob("h1", 10, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBlob("h1", 10, 5); err != nil {
		t.Fatal("duplicate AddBlob must be a no-op, got", err)
	}
	missing, err := s.MissingBlobs([]string{"h1", "h2"})
	if err != nil || len(missing) != 1 || missing[0] != "h2" {
		t.Fatalf("MissingBlobs: %v %v", missing, err)
	}
	st, err := s.Stats()
	if err != nil || st.Blobs != 1 || st.SizeRaw != 10 || st.SizeStored != 5 {
		t.Fatalf("Stats: %+v %v", st, err)
	}
}
