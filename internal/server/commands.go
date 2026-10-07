package server

import (
	"net/http"
	"strconv"
	"strings"

	"centralbackup/internal/proto"
	"centralbackup/internal/server/store"
)

// Remote command console: the operator runs a shell command on a client and
// watches its output stream back, live, in the server GUI. The agent runs it
// in its background service (as LocalSystem or root), so nothing shows on the
// client's screen. This is administrative remote execution, gated by the same
// admin session as everything else — the same trust already implied by
// job pre/post hooks and restore-to-arbitrary-path.

// maxCommandOutputPoll caps how many new output lines one poll returns.
const maxCommandOutputPoll = 2000

// shellForAgent validates the requested shell against the client's OS and
// fills in a default when none was given (PowerShell on Windows, sh else).
func shellForAgent(agentOS, shell string) (string, bool) {
	windows := strings.EqualFold(agentOS, "windows")
	switch shell {
	case "":
		if windows {
			return proto.ShellPowerShell, true
		}
		return proto.ShellSh, true
	case proto.ShellPowerShell, proto.ShellCmd:
		return shell, windows
	case proto.ShellSh:
		return shell, !windows
	default:
		return "", false
	}
}

func (s *Server) handleCommandRun(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	agent, err := s.store.GetAgent(agentID)
	if err != nil {
		httpError(w, http.StatusNotFound, "unknown client")
		return
	}
	req, err := decodeBody[struct {
		Shell   string `json:"shell"`
		Command string `json:"command"`
	}](r)
	if err != nil || strings.TrimSpace(req.Command) == "" {
		httpError(w, http.StatusBadRequest, "a command is required")
		return
	}
	shell, ok := shellForAgent(agent.OS, req.Shell)
	if !ok {
		httpError(w, http.StatusBadRequest, "that shell is not available on this client's OS")
		return
	}
	if !s.hub.Online(agentID) {
		httpError(w, http.StatusConflict, "client is offline")
		return
	}

	id := store.NewID()
	if err := s.store.CreateCommand(id, agentID, shell, req.Command); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if !s.hub.Send(agentID, proto.MsgRunCommand, proto.RunCommand{
		CommandID: id, Shell: shell, Command: req.Command,
	}) {
		s.store.FinishCommand(id, proto.RunError, -1, "failed to dispatch to client")
		httpError(w, http.StatusConflict, "could not reach the client")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"command_id": id, "shell": shell})
}

func (s *Server) handleCommandsList(w http.ResponseWriter, r *http.Request) {
	commands, err := s.store.ListCommands(r.URL.Query().Get("agent"), 50)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, commands)
}

// handleCommandGet returns the command plus any output lines after ?since=,
// so the console can poll for just what is new.
func (s *Server) handleCommandGet(w http.ResponseWriter, r *http.Request) {
	cmd, err := s.store.GetCommand(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "command not found")
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	lines, err := s.store.CommandOutput(cmd.ID, since, maxCommandOutputPoll)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": cmd, "output": lines})
}

func (s *Server) handleCommandCancel(w http.ResponseWriter, r *http.Request) {
	cmd, err := s.store.GetCommand(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "command not found")
		return
	}
	if cmd.Status != proto.RunRunning {
		httpError(w, http.StatusConflict, "command is not running")
		return
	}
	if !s.hub.Send(cmd.AgentID, proto.MsgCancelCommand, proto.CancelCommand{CommandID: cmd.ID}) {
		httpError(w, http.StatusConflict, "client is offline; cannot reach it to cancel")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCommandDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteCommand(r.PathValue("id")); err != nil {
		httpError(w, http.StatusInternalServerError, "could not delete command")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) commandBelongsToAgent(id, agentID string) bool {
	c, err := s.store.GetCommand(id)
	return err == nil && c.AgentID == agentID
}
