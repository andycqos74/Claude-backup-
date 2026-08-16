package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"centralbackup/internal/proto"
	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	backend, err := storage.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{store: st, storageActive: backend, storageBackendID: "local", hub: newHub()}
}

// putManifest stores a manifest referencing the given hashes and records
// the snapshot.
func putManifest(t *testing.T, s *Server, snapID, jobID string, hashes ...string) {
	t.Helper()
	var buf bytes.Buffer
	enc, _ := zstd.NewWriter(&buf)
	je := json.NewEncoder(enc)
	for i, h := range hashes {
		je.Encode(proto.ManifestEntry{Type: "f", Path: fmt.Sprintf("/f%d", i), Hash: h, Size: 1})
	}
	enc.Close()
	key := storage.ManifestKey(snapID)
	if _, err := s.backend().Put(key, &buf); err != nil {
		t.Fatal(err)
	}
	err := s.store.CreateSnapshot(store.Snapshot{
		ID: snapID, Backend: s.backendKey(), JobID: jobID, AgentID: "a1", Mode: proto.ModeFull,
		Files: int64(len(hashes)), ManifestKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func addBlob(t *testing.T, s *Server, hash string) {
	t.Helper()
	if _, err := s.backend().Put(storage.BlobKey(hash), bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AddBlob(s.backendKey(), hash, 1, 1); err != nil {
		t.Fatal(err)
	}
}

func TestGCBlobs(t *testing.T) {
	s := testServer(t)
	old := blobGCGrace
	blobGCGrace = 0
	defer func() { blobGCGrace = old }()

	h := func(i int) string {
		return fmt.Sprintf("%064d", i)
	}
	addBlob(t, s, h(1))
	addBlob(t, s, h(2))
	addBlob(t, s, h(3))
	putManifest(t, s, "snap1", "job1", h(1), h(2))

	if err := s.gcBlobs(); err != nil {
		t.Fatal(err)
	}
	blobs, _ := s.store.AllBlobs("local")
	if len(blobs) != 2 || blobs[h(3)] != 0 && len(blobs) == 3 {
		t.Fatalf("expected blob %s swept, have %v", h(3), blobs)
	}
	if _, ok := blobs[h(1)]; !ok {
		t.Fatal("referenced blob swept")
	}
	if ok, _ := s.backend().Has(storage.BlobKey(h(3))); ok {
		t.Fatal("swept blob still in storage")
	}
	if ok, _ := s.backend().Has(storage.BlobKey(h(1))); !ok {
		t.Fatal("referenced blob removed from storage")
	}
}

func TestGCGracePeriod(t *testing.T) {
	s := testServer(t)
	old := blobGCGrace
	blobGCGrace = time.Hour // fresh blobs must survive
	defer func() { blobGCGrace = old }()

	h := fmt.Sprintf("%064d", 9)
	addBlob(t, s, h) // unreferenced but fresh
	if err := s.gcBlobs(); err != nil {
		t.Fatal(err)
	}
	blobs, _ := s.store.AllBlobs("local")
	if _, ok := blobs[h]; !ok {
		t.Fatal("fresh unreferenced blob swept inside grace period")
	}
}

func TestPruneRetentionKeepLast(t *testing.T) {
	s := testServer(t)
	job := proto.Job{
		ID: "job1", AgentID: "a1", Name: "j", Paths: []string{"/x"},
		Enabled: true, Origin: proto.OriginServer, Version: 1,
		KeepLast: 2,
	}
	if err := s.store.SaveJob(job); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		putManifest(t, s, fmt.Sprintf("snap%d", i), "job1", fmt.Sprintf("%064d", i))
	}
	if err := s.pruneRetention(); err != nil {
		t.Fatal(err)
	}
	snaps, _ := s.store.ListSnapshots("local", "", "job1")
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots after prune, have %d", len(snaps))
	}
	// The newest two (snap4, snap3) must be the ones kept.
	if snaps[0].ID != "snap4" || snaps[1].ID != "snap3" {
		t.Fatalf("kept wrong snapshots: %s, %s", snaps[0].ID, snaps[1].ID)
	}
	// Their manifests must be gone from storage.
	if ok, _ := s.backend().Has(storage.ManifestKey("snap1")); ok {
		t.Fatal("pruned snapshot manifest still in storage")
	}
}

func TestPruneNoPolicyKeepsAll(t *testing.T) {
	s := testServer(t)
	job := proto.Job{
		ID: "job1", AgentID: "a1", Name: "j", Paths: []string{"/x"},
		Enabled: true, Origin: proto.OriginServer, Version: 1,
	}
	if err := s.store.SaveJob(job); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		putManifest(t, s, fmt.Sprintf("snap%d", i), "job1", fmt.Sprintf("%064d", i))
	}
	if err := s.pruneRetention(); err != nil {
		t.Fatal(err)
	}
	snaps, _ := s.store.ListSnapshots("local", "", "job1")
	if len(snaps) != 3 {
		t.Fatalf("no-policy job lost snapshots: %d left", len(snaps))
	}
}

func TestSnapshotTreeAndZipHelpers(t *testing.T) {
	s := testServer(t)
	var buf bytes.Buffer
	enc, _ := zstd.NewWriter(&buf)
	je := json.NewEncoder(enc)
	je.Encode(proto.ManifestEntry{Type: "d", Path: "/home/u"})
	je.Encode(proto.ManifestEntry{Type: "f", Path: "/home/u/a.txt", Size: 3, Hash: "h1"})
	je.Encode(proto.ManifestEntry{Type: "f", Path: "/home/u/sub/b.txt", Size: 4, Hash: "h2"})
	enc.Close()
	key := storage.ManifestKey("snapT")
	s.backend().Put(key, &buf)
	sn := &store.Snapshot{ID: "snapT", ManifestKey: key}

	root, err := s.snapshotTree(sn, "")
	if err != nil || len(root) != 1 || root[0].Name != "home" || root[0].Type != "d" {
		t.Fatalf("root tree: %+v %v", root, err)
	}
	lvl, err := s.snapshotTree(sn, "home/u")
	if err != nil || len(lvl) != 2 {
		t.Fatalf("home/u tree: %+v %v", lvl, err)
	}
	// Dirs sort first.
	if lvl[0].Name != "sub" || lvl[0].Type != "d" || lvl[1].Name != "a.txt" || lvl[1].Type != "f" {
		t.Fatalf("tree order/types wrong: %+v", lvl)
	}

	if !underPrefix("/home/u/a.txt", []string{"home/u"}) || underPrefix("/home/v/x", []string{"home/u"}) {
		t.Fatal("underPrefix misbehaves")
	}
}
