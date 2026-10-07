package server

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"centralbackup/internal/proto"
)

func TestShellForAgent(t *testing.T) {
	cases := []struct {
		os, shell, want string
		ok              bool
	}{
		{"windows", "", proto.ShellPowerShell, true},  // default on Windows
		{"windows", "powershell", "powershell", true}, //
		{"windows", "cmd", "cmd", true},               //
		{"windows", "sh", "", false},                  // sh not on Windows
		{"linux", "", proto.ShellSh, true},            // default elsewhere
		{"linux", "sh", "sh", true},                   //
		{"linux", "powershell", "", false},            // powershell only on Windows
		{"darwin", "cmd", "", false},                  //
		{"windows", "bash", "", false},                // unknown shell
	}
	for _, c := range cases {
		got, ok := shellForAgent(c.os, c.shell)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("shellForAgent(%q,%q) = (%q,%v), want (%q,%v)",
				c.os, c.shell, got, ok, c.want, c.ok)
		}
	}
}

func TestCommandRunRejectsWrongShell(t *testing.T) {
	s := testServer(t)
	// A Windows client cannot be asked to run sh.
	agentID, _, _ := s.store.CreateAgent("win", proto.EnrollRequest{Hostname: "win", OS: "windows"})
	r := httptest.NewRequest("POST", "/api/admin/agents/"+agentID+"/commands",
		bytes.NewReader([]byte(`{"shell":"sh","command":"ls"}`)))
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	s.handleCommandRun(w, r)
	if w.Code != 400 {
		t.Errorf("sh on Windows: status %d, want 400", w.Code)
	}
}

func TestCommandRunOfflineConflict(t *testing.T) {
	s := testServer(t)
	// Valid shell, but the client is offline (no hub connection in this test).
	agentID, _, _ := s.store.CreateAgent("lin", proto.EnrollRequest{Hostname: "lin", OS: "linux"})
	r := httptest.NewRequest("POST", "/api/admin/agents/"+agentID+"/commands",
		bytes.NewReader([]byte(`{"command":"uptime"}`)))
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	s.handleCommandRun(w, r)
	if w.Code != 409 {
		t.Errorf("offline client: status %d, want 409", w.Code)
	}
	// No command row should be left behind running for an undispatched command.
	cmds, _ := s.store.ListCommands(agentID, 10)
	if len(cmds) != 0 {
		t.Errorf("offline run should not create a command row, got %d", len(cmds))
	}
}

func TestCommandRunRequiresCommand(t *testing.T) {
	s := testServer(t)
	agentID, _, _ := s.store.CreateAgent("lin", proto.EnrollRequest{Hostname: "lin", OS: "linux"})
	r := httptest.NewRequest("POST", "/api/admin/agents/"+agentID+"/commands",
		bytes.NewReader([]byte(`{"command":"   "}`)))
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	s.handleCommandRun(w, r)
	if w.Code != 400 {
		t.Errorf("blank command: status %d, want 400", w.Code)
	}
}

func TestCommandGetIncremental(t *testing.T) {
	s := testServer(t)
	agentID, _, _ := s.store.CreateAgent("lin", proto.EnrollRequest{Hostname: "lin", OS: "linux"})
	s.store.CreateCommand("c1", agentID, proto.ShellSh, "echo hi")
	s.store.AppendCommandOutput("c1", "stdout", "hello")
	s.store.AppendCommandOutput("c1", "stdout", "world")

	// First poll from seq 0 returns both lines.
	r := httptest.NewRequest("GET", "/api/admin/commands/c1?since=0", nil)
	r.SetPathValue("id", "c1")
	w := httptest.NewRecorder()
	s.handleCommandGet(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if n := bytes.Count(w.Body.Bytes(), []byte(`"line"`)); n != 2 {
		t.Errorf("expected 2 output lines, saw %d in %s", n, w.Body.String())
	}
}
