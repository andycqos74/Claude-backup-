package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeDrive is a minimal Google Drive emulator covering the endpoints the
// googleDrive backend uses: files.list query, resumable upload
// (init + chunked PUT to a session URL), download (alt=media), delete, and
// folder creation.
type fakeDrive struct {
	mu       sync.Mutex
	nextID   int
	files    map[string]*fakeDriveFile // id -> file
	sessions map[string]*fakeDriveSess // session id -> upload
	srv      *httptest.Server
}

type fakeDriveFile struct {
	id, name, parent, mime string
	content                []byte
}

type fakeDriveSess struct {
	name, parent string
	buf          []byte
}

func newFakeDrive(t *testing.T) *fakeDrive {
	d := &fakeDrive{files: map[string]*fakeDriveFile{}, sessions: map[string]*fakeDriveSess{}}
	mux := http.NewServeMux()

	// files.list, create-folder (POST /drive/v3/files), download & delete
	// (/drive/v3/files/{id}).
	mux.HandleFunc("/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch r.Method {
		case "GET": // files.list?q=...
			q := r.URL.Query().Get("q")
			name := extractEq(q, "name = ")
			var parent string
			if i := strings.Index(q, " in parents"); i >= 0 {
				// "<parent>' in parents" — parent is the quoted token before
				before := strings.TrimSpace(q[:i])
				parent = unquote(lastToken(before))
			}
			wantFolder := strings.Contains(q, "application/vnd.google-apps.folder")
			for _, f := range d.files {
				if f.name == name && (parent == "" || f.parent == parent) &&
					(!wantFolder || f.mime == "application/vnd.google-apps.folder") {
					writeJSONList(w, f.id)
					return
				}
			}
			writeJSONList(w) // empty
		case "POST": // create folder (metadata-only)
			var meta struct {
				Name, MimeType string
				Parents        []string
			}
			json.NewDecoder(r.Body).Decode(&meta)
			id := d.mint()
			parent := ""
			if len(meta.Parents) > 0 {
				parent = meta.Parents[0]
			}
			d.files[id] = &fakeDriveFile{id: id, name: meta.Name, parent: parent, mime: meta.MimeType}
			fmt.Fprintf(w, `{"id":%q}`, id)
		default:
			w.WriteHeader(400)
		}
	})

	mux.HandleFunc("/drive/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		d.mu.Lock()
		defer d.mu.Unlock()
		f, ok := d.files[id]
		switch {
		case r.Method == "GET" && r.URL.Query().Get("alt") == "media":
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Write(f.content)
		case r.Method == "DELETE":
			if !ok {
				w.WriteHeader(404)
				return
			}
			delete(d.files, id)
			w.WriteHeader(204)
		default:
			w.WriteHeader(400)
		}
	})

	// Resumable upload init.
	mux.HandleFunc("/upload/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		var meta struct {
			Name    string
			Parents []string
		}
		json.NewDecoder(r.Body).Decode(&meta)
		d.mu.Lock()
		sid := strconv.Itoa(len(d.sessions) + 1)
		parent := ""
		if len(meta.Parents) > 0 {
			parent = meta.Parents[0]
		}
		d.sessions[sid] = &fakeDriveSess{name: meta.Name, parent: parent}
		d.mu.Unlock()
		w.Header().Set("Location", d.srv.URL+"/upload/session/"+sid)
		w.WriteHeader(200)
	})

	// Resumable chunk PUTs.
	mux.HandleFunc("/upload/session/", func(w http.ResponseWriter, r *http.Request) {
		sid := strings.TrimPrefix(r.URL.Path, "/upload/session/")
		d.mu.Lock()
		defer d.mu.Unlock()
		sess, ok := d.sessions[sid]
		if !ok {
			w.WriteHeader(404)
			return
		}
		cr := r.Header.Get("Content-Range")
		var start, end, total int64
		if strings.HasPrefix(cr, "bytes */") {
			fmt.Sscanf(cr, "bytes */%d", &total)
		} else {
			fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total)
			chunk, _ := io.ReadAll(r.Body)
			sess.buf = append(sess.buf, chunk...)
		}
		if int64(len(sess.buf)) >= total { // done
			id := d.mint()
			d.files[id] = &fakeDriveFile{id: id, name: sess.name, parent: sess.parent, content: sess.buf}
			delete(d.sessions, sid)
			fmt.Fprintf(w, `{"id":%q}`, id)
			return
		}
		w.WriteHeader(308) // resume incomplete
	})

	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDrive) mint() string {
	d.nextID++
	return "file" + strconv.Itoa(d.nextID)
}

func writeJSONList(w http.ResponseWriter, ids ...string) {
	var files []map[string]string
	for _, id := range ids {
		files = append(files, map[string]string{"id": id})
	}
	json.NewEncoder(w).Encode(map[string]any{"files": files})
}

// extractEq pulls the single-quoted value following a "name = " clause.
func extractEq(q, prefix string) string {
	i := strings.Index(q, prefix)
	if i < 0 {
		return ""
	}
	rest := q[i+len(prefix):]
	return unquote(firstToken(rest))
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "'") {
		return ""
	}
	end := strings.Index(s[1:], "'")
	if end < 0 {
		return s
	}
	return s[:end+2]
}

func lastToken(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasSuffix(s, "'") {
		return ""
	}
	start := strings.LastIndex(s[:len(s)-1], "'")
	if start < 0 {
		return s
	}
	return s[start:]
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "'")
	s = strings.TrimSuffix(s, "'")
	s = strings.ReplaceAll(s, `\'`, `'`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

func newTestGoogleDrive(d *fakeDrive) *googleDrive {
	g := newGoogleDrive(Config{Folder: "cb-test"}, func(ctx context.Context) (string, error) {
		return "test-token", nil
	}, d.srv.Client())
	g.apiBase = d.srv.URL + "/drive/v3"
	g.uploadBase = d.srv.URL + "/upload/drive/v3"
	return g
}

func TestGoogleDriveRoundTripSmall(t *testing.T) {
	d := newFakeDrive(t)
	g := newTestGoogleDrive(d)
	runBackendContract(t, g, []byte("hello google drive"))
}

func TestGoogleDriveRoundTripLarge(t *testing.T) {
	d := newFakeDrive(t)
	g := newTestGoogleDrive(d)
	big := make([]byte, 20<<20) // 20 MiB -> multiple 8 MiB chunks
	for i := range big {
		big[i] = byte(i * 13)
	}
	runBackendContract(t, g, big)
}

func TestGoogleDriveReusesFolder(t *testing.T) {
	d := newFakeDrive(t)
	g := newTestGoogleDrive(d)
	// Two puts must resolve/create the folder only once.
	if _, err := g.Put(BlobKey(strings.Repeat("a", 64)), strings.NewReader("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Put(BlobKey(strings.Repeat("b", 64)), strings.NewReader("two")); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	folders := 0
	for _, f := range d.files {
		if f.mime == "application/vnd.google-apps.folder" {
			folders++
		}
	}
	d.mu.Unlock()
	if folders != 1 {
		t.Fatalf("expected exactly 1 destination folder, got %d", folders)
	}
}
