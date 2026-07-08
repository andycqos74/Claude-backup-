package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUploadFileCancelDoesNotHang verifies that cancelling the context
// during an in-flight upload causes uploadFile to return promptly, instead
// of deadlocking on the io.Pipe used to stream the compressed body. A
// server that never reads the request body (simulating one stalled
// mid-read) reproduces the exact condition where relying solely on
// net/http to propagate ctx cancellation into a custom io.Reader body is
// not enough: the pipe must be closed explicitly.
func TestUploadFileCancelDoesNotHang(t *testing.T) {
	reqStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(reqStarted)
		<-r.Context().Done() // never read the body, never respond
	}))
	// Not srv.Close(): that blocks until the handler above returns, which
	// only happens once the connection is actually torn down -- a real but
	// slow (~seconds) OS/runtime-level teardown, unrelated to what this
	// test is verifying. Force-close client connections instead so the
	// test doesn't pay for that.
	defer srv.CloseClientConnections()

	path := filepath.Join(t.TempDir(), "big.bin")
	// Large enough that the compressor is still feeding the pipe when we
	// cancel, rather than having already finished.
	if err := os.WriteFile(path, make([]byte, 8<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{client: &serverClient{
		base:  srv.URL,
		creds: &Credentials{AgentID: "a", Secret: "s"},
		http:  srv.Client(),
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-reqStarted
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, _, err := a.uploadFile(ctx, path, "deadbeef")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a cancelled upload")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("uploadFile did not return within 5s of cancellation -- deadlocked on the pipe")
	}
}
