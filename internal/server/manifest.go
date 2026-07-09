package server

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"centralbackup/internal/proto"
	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

// Manifest paths always use forward slashes, including on Windows
// ("C:/Users/..."); agents normalise with filepath.ToSlash when scanning
// and convert back when restoring.

// eachManifestEntry streams a snapshot manifest, invoking fn per entry.
func (s *Server) eachManifestEntry(manifestKey string, fn func(e proto.ManifestEntry) error) error {
	rc, err := s.backend().Get(manifestKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	dec, err := zstd.NewReader(rc)
	if err != nil {
		return err
	}
	defer dec.Close()
	sc := bufio.NewScanner(dec)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e proto.ManifestEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("corrupt manifest line: %w", err)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return sc.Err()
}

// TreeNode is one child in the snapshot file browser.
type TreeNode struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Type  string `json:"type"` // f | d | l
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // unix seconds
}

// snapshotTree lists the immediate children of prefix within a snapshot's
// manifest. An empty prefix lists the roots (e.g. "C:" or "home").
func (s *Server) snapshotTree(sn *store.Snapshot, prefix string) ([]TreeNode, error) {
	prefix = strings.Trim(prefix, "/")
	nodes := map[string]*TreeNode{}
	err := s.eachManifestEntry(sn.ManifestKey, func(e proto.ManifestEntry) error {
		p := strings.TrimPrefix(strings.TrimLeft(e.Path, "/"), "/")
		if prefix != "" {
			if !strings.HasPrefix(p, prefix+"/") {
				return nil
			}
			p = p[len(prefix)+1:]
		}
		if p == "" {
			return nil
		}
		name, rest, isLeaf := p, "", true
		if i := strings.IndexByte(p, '/'); i >= 0 {
			name, rest, isLeaf = p[:i], p[i+1:], false
		}
		_ = rest
		full := name
		if prefix != "" {
			full = prefix + "/" + name
		}
		if isLeaf {
			nodes[name] = &TreeNode{Name: name, Path: full, Type: e.Type, Size: e.Size, Mtime: e.Mtime / 1e9}
		} else if _, ok := nodes[name]; !ok {
			nodes[name] = &TreeNode{Name: name, Path: full, Type: "d"}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]TreeNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == "d") != (out[j].Type == "d") {
			return out[i].Type == "d"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// underPrefix reports whether a manifest path falls under any of the given
// slash-form prefixes (nil/empty = everything matches).
func underPrefix(path string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	p := strings.TrimLeft(path, "/")
	for _, pre := range prefixes {
		pre = strings.Trim(pre, "/")
		if pre == "" || p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// streamFile writes one file's decompressed content from blob storage.
func (s *Server) streamFile(w io.Writer, hash string) error {
	rc, err := s.backend().Get(storage.BlobKey(hash))
	if err != nil {
		return err
	}
	defer rc.Close()
	dec, err := zstd.NewReader(rc)
	if err != nil {
		return err
	}
	defer dec.Close()
	_, err = io.Copy(w, dec.IOReadCloser())
	return err
}

// writeSnapshotZip streams selected snapshot contents as a zip archive.
func (s *Server) writeSnapshotZip(w io.Writer, sn *store.Snapshot, prefixes []string) error {
	zw := zip.NewWriter(w)
	err := s.eachManifestEntry(sn.ManifestKey, func(e proto.ManifestEntry) error {
		if e.Type != "f" || !underPrefix(e.Path, prefixes) {
			return nil
		}
		name := strings.TrimLeft(e.Path, "/")
		name = strings.ReplaceAll(name, ":", "") // strip Windows drive colon
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		hdr.Modified = time.Unix(0, e.Mtime)
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		return s.streamFile(fw, e.Hash)
	})
	if err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}
