package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"

	"github.com/klauspost/compress/zstd"

	"centralbackup/internal/proto"
	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

// putTreeManifest stores a manifest of the given paths, each with its own
// one-byte blob, and returns the snapshot to zip.
func putTreeManifest(t *testing.T, s *Server, snapID string, paths []string) *store.Snapshot {
	t.Helper()
	var buf bytes.Buffer
	enc, _ := zstd.NewWriter(&buf)
	je := json.NewEncoder(enc)
	for i, p := range paths {
		hash := hashForIndex(i)
		je.Encode(proto.ManifestEntry{Type: "f", Path: p, Hash: hash, Size: 1})

		var blob bytes.Buffer
		bw, _ := zstd.NewWriter(&blob)
		bw.Write([]byte{byte('a' + i%26)})
		bw.Close()
		if _, err := s.backend().Put(storage.BlobKey(hash), bytes.NewReader(blob.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	enc.Close()

	key := storage.ManifestKey(snapID)
	if _, err := s.backend().Put(key, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	return &store.Snapshot{ID: snapID, ManifestKey: key}
}

func hashForIndex(i int) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, 64)
	for j := range out {
		out[j] = hexd[(i+j)%16]
	}
	return string(out)
}

func zipNames(t *testing.T, data []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("archive is not a valid zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}

// A selection spanning several folders must produce every matching file,
// not just the first one.
func TestSnapshotZipIncludesEverySelectedPath(t *testing.T) {
	s := testServer(t)
	files := []string{
		"E:/Users/Andy/Documents/Websites/Source 2024/.vs/config.json",
		"E:/Users/Andy/Documents/Websites/Source 2024/.vs/state.bin",
		"E:/Users/Andy/Documents/Websites/Source 2024/2024menu/menu.css",
		"E:/Users/Andy/Documents/Websites/Source 2024/css/site.css",
		"E:/Users/Andy/Documents/Websites/Source 2024/js/app.js",
		"E:/Users/Andy/Documents/Websites/Source 2024/untouched/other.txt",
	}
	sn := putTreeManifest(t, s, "snap1", files)

	prefixes := []string{
		"E:/Users/Andy/Documents/Websites/Source 2024/.vs",
		"E:/Users/Andy/Documents/Websites/Source 2024/2024menu",
		"E:/Users/Andy/Documents/Websites/Source 2024/css",
		"E:/Users/Andy/Documents/Websites/Source 2024/js",
	}

	var out bytes.Buffer
	if err := s.writeSnapshotZip(&out, sn, prefixes); err != nil {
		t.Fatalf("writeSnapshotZip: %v", err)
	}
	names := zipNames(t, out.Bytes())
	if len(names) != 5 {
		t.Errorf("zip has %d entries, want 5 (two under .vs plus one each from 2024menu, css, js)\ngot: %v",
			len(names), names)
	}
	for _, n := range names {
		if bytes.Contains([]byte(n), []byte("untouched")) {
			t.Errorf("unselected folder was included: %s", n)
		}
	}
}

// No selection means the whole snapshot.
func TestSnapshotZipWithNoSelectionIncludesEverything(t *testing.T) {
	s := testServer(t)
	files := []string{"C:/a/one.txt", "C:/b/two.txt", "C:/b/c/three.txt"}
	sn := putTreeManifest(t, s, "snap2", files)

	var out bytes.Buffer
	if err := s.writeSnapshotZip(&out, sn, nil); err != nil {
		t.Fatalf("writeSnapshotZip: %v", err)
	}
	if names := zipNames(t, out.Bytes()); len(names) != 3 {
		t.Errorf("zip has %d entries, want 3: %v", len(names), names)
	}
}

// A single selected file must not drag in its siblings: the prefix match is
// on whole path segments, not a plain string prefix.
func TestSnapshotZipPrefixMatchesWholeSegments(t *testing.T) {
	s := testServer(t)
	files := []string{"C:/data/report.txt", "C:/data-archive/old.txt", "C:/data/sub/x.txt"}
	sn := putTreeManifest(t, s, "snap3", files)

	var out bytes.Buffer
	if err := s.writeSnapshotZip(&out, sn, []string{"C:/data"}); err != nil {
		t.Fatal(err)
	}
	names := zipNames(t, out.Bytes())
	for _, n := range names {
		if bytes.Contains([]byte(n), []byte("data-archive")) {
			t.Errorf("sibling folder matched by prefix: %s", n)
		}
	}
	if len(names) != 2 {
		t.Errorf("zip has %d entries, want 2: %v", len(names), names)
	}
}

// The selection has to survive the URL round trip, which is where this
// actually broke: it was sent as one parameter with NUL separators, and a
// stripped NUL silently reduced the zip to the first folder.
func TestSnapshotDownloadZipParsesRepeatedPathParams(t *testing.T) {
	s := testServer(t)
	files := []string{"C:/a/one.txt", "C:/b/two.txt", "C:/c/three.txt"}
	sn := putTreeManifest(t, s, "snap4", files)
	if err := s.store.CreateSnapshot(store.Snapshot{
		ID: sn.ID, Backend: s.backendKey(), JobID: "j1", AgentID: "a1",
		Mode: proto.ModeFull, Files: int64(len(files)), ManifestKey: sn.ManifestKey,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET",
		"/api/admin/snapshots/snap4/download?zip=1"+
			"&path="+url.QueryEscape("C:/a")+
			"&path="+url.QueryEscape("C:/b"), nil)
	req.SetPathValue("id", "snap4")
	rec := httptest.NewRecorder()
	s.handleSnapshotDownload(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	names := zipNames(t, rec.Body.Bytes())
	if len(names) != 2 {
		t.Errorf("zip has %d entries, want 2 (a and b, not c): %v", len(names), names)
	}
}
