package server

import (
	"path/filepath"
	"testing"

	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

// TestBackendSwitchReuploads reproduces the reported bug: after a blob is
// recorded under one backend, switching to a different backend must report
// that blob as MISSING again (so the agent re-uploads it), rather than
// assuming it already exists — which previously caused "blob not found" on
// the new backend.
func TestBackendSwitchReuploads(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	local, _ := storage.NewLocalFS(filepath.Join(dir, "local"))
	s := &Server{store: st, hub: newHub()}
	s.setBackend(local, storage.Config{Provider: storage.ProviderLocal})

	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Record the blob under the local backend (as a completed upload would).
	if err := st.AddBlob(s.backendKey(), hash, 10, 5); err != nil {
		t.Fatal(err)
	}
	if missing, _ := st.MissingBlobs(s.backendKey(), []string{hash}); len(missing) != 0 {
		t.Fatal("blob should be present in the local backend")
	}

	// Switch to a cloud-style backend (different backend key).
	cloud, _ := storage.NewLocalFS(filepath.Join(dir, "cloud")) // stand-in backend
	s.setBackend(cloud, storage.Config{Provider: storage.ProviderOneDrive, Account: "a@b.com", Folder: "cb"})

	// The same hash must now be reported missing so it gets re-uploaded.
	missing, err := st.MissingBlobs(s.backendKey(), []string{hash})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != hash {
		t.Fatalf("after backend switch, blob must be missing (re-uploaded); got %v", missing)
	}

	// And a job's latest snapshot is per-backend, so the first backup after
	// the switch is treated as full.
	if err := st.CreateSnapshot(store.Snapshot{ID: "s-local", Backend: "local", JobID: "j1", AgentID: "a1", Mode: "full", ManifestKey: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LatestSnapshot(s.backendKey(), "j1"); err != store.ErrNotFound {
		t.Fatalf("new backend should have no prior snapshot for the job; got %v", err)
	}
}
