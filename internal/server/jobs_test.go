package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"centralbackup/internal/proto"
)

// newTestHubConn spins up a tiny websocket server backed by hub h, dials
// it, and registers the server-side connection under agentID. Returns the
// client-side connection for the test to read/write.
func newTestHubConn(t *testing.T, h *Hub, agentID string) *websocket.Conn {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		h.register(agentID, ws)
	}))
	t.Cleanup(srv.Close)

	url := "ws" + srv.URL[len("http"):]
	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCancelRunQueued(t *testing.T) {
	s := testServer(t)
	runID, err := s.store.CreateRun("job1", "a1", proto.ModeFull, proto.RunQueued)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cancelRun(runID); err != nil {
		t.Fatal(err)
	}
	run, err := s.store.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != proto.RunCancelled {
		t.Fatalf("status = %s, want cancelled", run.Status)
	}
}

func TestCancelRunOfflineAgent(t *testing.T) {
	s := testServer(t)
	runID, err := s.store.CreateRun("job1", "a1", proto.ModeFull, proto.RunRunning)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cancelRun(runID); err == nil {
		t.Fatal("expected error cancelling a running run with no connected agent")
	}
	// Status must be untouched -- cancelRun does not mark it finished
	// itself; that's the agent's job once it actually stops.
	run, _ := s.store.GetRun(runID)
	if run.Status != proto.RunRunning {
		t.Fatalf("status changed to %s, want unchanged running", run.Status)
	}
}

func TestCancelRunOnlineAgent(t *testing.T) {
	s := testServer(t)
	runID, err := s.store.CreateRun("job1", "a1", proto.ModeFull, proto.RunRunning)
	if err != nil {
		t.Fatal(err)
	}
	client := newTestHubConn(t, s.hub, "a1")
	waitFor(t, func() bool { return s.hub.Online("a1") })

	if err := s.cancelRun(runID); err != nil {
		t.Fatal(err)
	}
	// cancelRun must not itself finish the run -- only the agent's own
	// RunDone report does that, once it actually stops.
	run, _ := s.store.GetRun(runID)
	if run.Status != proto.RunRunning {
		t.Fatalf("cancelRun must not finish the run itself; status = %s", run.Status)
	}

	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	var env proto.Envelope
	if err := client.ReadJSON(&env); err != nil {
		t.Fatal(err)
	}
	if env.Type != proto.MsgCancelRun {
		t.Fatalf("message type = %s, want %s", env.Type, proto.MsgCancelRun)
	}
	var cancel proto.CancelRun
	if err := json.Unmarshal(env.Data, &cancel); err != nil {
		t.Fatal(err)
	}
	if cancel.RunID != runID {
		t.Fatalf("cancelled run id = %s, want %s", cancel.RunID, runID)
	}
}

func TestCancelRunNotInProgress(t *testing.T) {
	s := testServer(t)
	runID, err := s.store.CreateRun("job1", "a1", proto.ModeFull, proto.RunRunning)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(runID, proto.RunSuccess, "snap1", "", proto.RunStats{}); err != nil {
		t.Fatal(err)
	}
	if err := s.cancelRun(runID); err == nil {
		t.Fatal("expected error cancelling an already-finished run")
	}
}

func TestCancelRunUnknown(t *testing.T) {
	s := testServer(t)
	if err := s.cancelRun("does-not-exist"); err == nil {
		t.Fatal("expected error cancelling an unknown run")
	}
}
