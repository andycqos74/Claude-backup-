package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGraph is a minimal in-memory Microsoft Graph OneDrive emulator
// covering exactly the endpoints the oneDrive backend uses: simple upload,
// createUploadSession + chunked PUT, download, metadata (Has) and delete.
type fakeGraph struct {
	mu       sync.Mutex
	files    map[string][]byte      // path -> content
	sessions map[string]*fakeUpload // session id -> in-progress upload
	srv      *httptest.Server
	failNext map[string]int // path -> number of times to return 503 first
}

type fakeUpload struct {
	path string
	buf  []byte
}

func newFakeGraph(t *testing.T) *fakeGraph {
	g := &fakeGraph{
		files:    map[string][]byte{},
		sessions: map[string]*fakeUpload{},
		failNext: map[string]int{},
	}
	mux := http.NewServeMux()

	// Drive-item operations addressed by path: /me/drive/root:/<path>:/<action>
	mux.HandleFunc("/me/drive/root:/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/me/drive/root:/")
		// Split "<path>:/<action>" — the ":" separates the item path from
		// the action (content / createUploadSession), if present.
		path, action := rest, ""
		if i := strings.Index(rest, ":"); i >= 0 {
			path, action = rest[:i], strings.TrimPrefix(rest[i+1:], "/")
		}
		path = decodeTestPath(path)

		g.mu.Lock()
		defer g.mu.Unlock()
		if n := g.failNext[path+"|"+action]; n > 0 {
			g.failNext[path+"|"+action] = n - 1
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		switch {
		case r.Method == "PUT" && action == "content": // simple upload
			body, _ := io.ReadAll(r.Body)
			g.files[path] = body
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"%s","size":%d}`, path, len(body))

		case r.Method == "POST" && action == "createUploadSession":
			id := strconv.Itoa(len(g.sessions) + 1)
			g.sessions[id] = &fakeUpload{path: path}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"uploadUrl":"%s/upload/%s"}`, g.srv.URL, id)

		case r.Method == "GET" && action == "content": // download
			body, ok := g.files[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(body)

		case r.Method == "GET" && action == "": // metadata / Has
			if _, ok := g.files[path]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"id":"%s","size":%d}`, path, len(g.files[path]))

		case r.Method == "DELETE" && action == "":
			if _, ok := g.files[path]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(g.files, path)
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	// Chunked upload session PUTs.
	mux.HandleFunc("/upload/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/upload/")
		g.mu.Lock()
		defer g.mu.Unlock()
		sess, ok := g.sessions[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == "DELETE" {
			delete(g.sessions, id)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		cr := r.Header.Get("Content-Range") // bytes start-end/total
		var start, end, total int64
		fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total)
		chunk, _ := io.ReadAll(r.Body)
		sess.buf = append(sess.buf, chunk...)
		if end+1 >= total { // last chunk
			g.files[sess.path] = sess.buf
			delete(g.sessions, id)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":"%s","size":%d}`, sess.path, len(sess.buf))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// decodeTestPath reverses pathEscapeSegment for the segments the test uses
// (our keys are hex/./- so only rare chars would be escaped; handle %XX).
func decodeTestPath(p string) string {
	// net/http already decodes %XX in URL.Path, so p is plain here.
	return p
}

func newTestOneDrive(g *fakeGraph) *oneDrive {
	o := newOneDrive(Config{Folder: "cb-test"}, func(ctx context.Context) (string, error) {
		return "test-token", nil
	}, g.srv.Client())
	o.base = g.srv.URL
	return o
}

func TestOneDriveRoundTripSmall(t *testing.T) {
	g := newFakeGraph(t)
	o := newTestOneDrive(g)
	runBackendContract(t, o, []byte("hello onedrive"))
}

func TestOneDriveRoundTripLarge(t *testing.T) {
	g := newFakeGraph(t)
	o := newTestOneDrive(g)
	// 25 MiB forces the chunked upload-session path (chunk size 10 MiB).
	big := make([]byte, 25<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	runBackendContract(t, o, big)
}

func TestOneDriveRetriesTransientFailure(t *testing.T) {
	g := newFakeGraph(t)
	o := newTestOneDrive(g)
	key := BlobKey("aa" + strings.Repeat("b", 62))
	// Make the first two simple-upload attempts fail with 503.
	g.failNext["cb-test/"+key+"|content"] = 2
	if _, err := o.Put(key, strings.NewReader("retryable payload")); err != nil {
		t.Fatalf("Put should have succeeded after retries: %v", err)
	}
	rc, err := o.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "retryable payload" {
		t.Fatalf("content after retried upload = %q", b)
	}
}

// runBackendContract exercises the full Backend contract against any
// implementation: Has(false) -> Get(ErrNotFound) -> Put -> Has(true) ->
// Get(content) -> Delete -> Has(false) -> Delete(idempotent).
func runBackendContract(t *testing.T, b Backend, content []byte) {
	t.Helper()
	key := BlobKey(fmt.Sprintf("%064x", len(content)))

	if ok, err := b.Has(key); err != nil || ok {
		t.Fatalf("Has before Put: ok=%v err=%v", ok, err)
	}
	if _, err := b.Get(key); err != ErrNotFound {
		t.Fatalf("Get before Put: want ErrNotFound, got %v", err)
	}

	n, err := b.Put(key, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(content)) {
		t.Fatalf("Put returned %d, want %d", n, len(content))
	}

	if ok, err := b.Has(key); err != nil || !ok {
		t.Fatalf("Has after Put: ok=%v err=%v", ok, err)
	}
	rc, err := b.Get(key)
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("Get returned %d bytes, want %d (equal=%v)", len(got), len(content), bytes.Equal(got, content))
	}

	if err := b.Delete(key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := b.Has(key); ok {
		t.Fatal("Has after Delete = true")
	}
	if err := b.Delete(key); err != nil {
		t.Fatalf("Delete of missing object must be nil, got %v", err)
	}
}
