package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"centralbackup/internal/proto"
	"centralbackup/internal/server/store"
)

const sessionCookie = "cb_session"

func (s *Server) sessionUser(r *http.Request) *store.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	u, err := s.store.SessionUser(c.Value)
	if err != nil {
		return nil
	}
	return u
}

// adminAuth guards the admin API. State-changing methods additionally
// require the X-Requested-With header, which browsers won't attach to
// cross-site form submissions (CSRF defence on top of SameSite cookies).
func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.sessionUser(r) == nil {
			httpError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "fetch" {
			httpError(w, http.StatusForbidden, "missing request header")
			return
		}
		next(w, r)
	}
}

func decodeBody[T any](r *http.Request) (T, error) {
	var v T
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&v)
	return v, err
}

func (s *Server) registerAdminAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/setup", s.handleSetup)
	mux.HandleFunc("POST /api/admin/login", s.handleLogin)
	mux.HandleFunc("POST /api/admin/logout", s.handleLogout)

	mux.HandleFunc("GET /api/admin/overview", s.adminAuth(s.handleOverview))
	mux.HandleFunc("GET /api/admin/agents", s.adminAuth(s.handleAgentsList))
	mux.HandleFunc("POST /api/admin/agents/{id}/rename", s.adminAuth(s.handleAgentRename))
	mux.HandleFunc("DELETE /api/admin/agents/{id}", s.adminAuth(s.handleAgentDelete))
	mux.HandleFunc("POST /api/admin/tokens", s.adminAuth(s.handleTokenCreate))
	mux.HandleFunc("GET /api/admin/jobs", s.adminAuth(s.handleJobsList))
	mux.HandleFunc("POST /api/admin/jobs", s.adminAuth(s.handleJobSave))
	mux.HandleFunc("DELETE /api/admin/jobs/{id}", s.adminAuth(s.handleJobDelete))
	mux.HandleFunc("POST /api/admin/jobs/{id}/run", s.adminAuth(s.handleJobRun))
	mux.HandleFunc("GET /api/admin/runs", s.adminAuth(s.handleRunsList))
	mux.HandleFunc("GET /api/admin/runs/{id}", s.adminAuth(s.handleRunGet))
	mux.HandleFunc("POST /api/admin/runs/{id}/cancel", s.adminAuth(s.handleRunCancel))
	mux.HandleFunc("GET /api/admin/snapshots", s.adminAuth(s.handleSnapshotsList))
	mux.HandleFunc("GET /api/admin/snapshots/{id}/tree", s.adminAuth(s.handleSnapshotTree))
	mux.HandleFunc("POST /api/admin/snapshots/{id}/restore", s.adminAuth(s.handleSnapshotRestore))
	mux.HandleFunc("GET /api/admin/snapshots/{id}/download", s.adminAuth(s.handleSnapshotDownload))
	mux.HandleFunc("DELETE /api/admin/snapshots/{id}", s.adminAuth(s.handleSnapshotDelete))
	mux.HandleFunc("POST /api/admin/gc", s.adminAuth(s.handleGC))
	mux.HandleFunc("POST /api/admin/password", s.adminAuth(s.handlePassword))
	mux.HandleFunc("GET /api/admin/settings", s.adminAuth(s.handleSettings))
}

// ---- auth ----

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountUsers()
	if err != nil || n > 0 {
		httpError(w, http.StatusForbidden, "setup already completed")
		return
	}
	req, err := decodeBody[struct{ Username, Password string }](r)
	if err != nil || req.Username == "" || len(req.Password) < 8 {
		httpError(w, http.StatusBadRequest, "username and a password of at least 8 characters are required")
		return
	}
	if err := s.store.CreateUser(req.Username, req.Password); err != nil {
		httpError(w, http.StatusInternalServerError, "could not create user")
		return
	}
	s.issueSession(w, r, req.Username, req.Password)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	req, err := decodeBody[struct{ Username, Password string }](r)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	// Modest brake on password guessing.
	time.Sleep(300 * time.Millisecond)
	s.issueSession(w, r, req.Username, req.Password)
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, username, password string) {
	u, err := s.store.Authenticate(username, password)
	if err != nil {
		httpError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	token, err := s.store.CreateSession(u.ID, 7*24*time.Hour)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "session error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int((7 * 24 * time.Hour).Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]string{"username": u.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	req, err := decodeBody[struct{ Old, New string }](r)
	if err != nil || len(req.New) < 8 {
		httpError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	u := s.sessionUser(r)
	if _, err := s.store.Authenticate(u.Username, req.Old); err != nil {
		httpError(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	if err := s.store.SetPassword(u.ID, req.New); err != nil {
		httpError(w, http.StatusInternalServerError, "could not update password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- agents & enrollment ----

type agentJSON struct {
	store.Agent
	Online bool `json:"online"`
}

func (s *Server) agentsJSON() ([]agentJSON, error) {
	agents, err := s.store.ListAgents()
	if err != nil {
		return nil, err
	}
	online := s.hub.OnlineIDs()
	out := make([]agentJSON, 0, len(agents))
	for _, a := range agents {
		out = append(out, agentJSON{Agent: a, Online: online[a.ID]})
	}
	return out, nil
}

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	out, err := s.agentsJSON()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAgentRename(w http.ResponseWriter, r *http.Request) {
	req, err := decodeBody[struct{ Name string }](r)
	if err != nil || strings.TrimSpace(req.Name) == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.store.RenameAgent(r.PathValue("id"), strings.TrimSpace(req.Name)); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAgentDelete removes an agent along with its jobs, runs and
// snapshots (manifests deleted now, blob data on next GC).
func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snaps, err := s.store.ListSnapshots(id, "")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	for _, sn := range snaps {
		if err := s.deleteSnapshot(sn.ID); err != nil {
			httpError(w, http.StatusInternalServerError, "snapshot delete failed")
			return
		}
	}
	if err := s.store.DeleteAgent(id); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	req, _ := decodeBody[struct{ Note string }](r)
	token, err := s.store.CreateEnrollToken(req.Note, 24*time.Hour)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"token":       token,
		"fingerprint": s.fingerprint,
	})
}

// ---- jobs ----

func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListJobs(r.URL.Query().Get("agent"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	jobs := make([]proto.Job, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, row.Job)
	}
	writeJSON(w, http.StatusOK, jobs)
}

// handleJobSave creates or updates a job from the GUI. Server edits bump
// the version so they win LWW against stale client copies, then the new
// job set is pushed to the agent.
func (s *Server) handleJobSave(w http.ResponseWriter, r *http.Request) {
	job, err := decodeBody[proto.Job](r)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad request body")
		return
	}
	if err := validateJob(&job); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.store.GetAgent(job.AgentID); err != nil {
		httpError(w, http.StatusBadRequest, "unknown agent")
		return
	}
	if job.ID == "" {
		job.ID = store.NewID()
		job.Origin = proto.OriginServer
		job.Version = 1
	} else {
		cur, err := s.store.GetJob(job.ID)
		if err != nil {
			httpError(w, http.StatusNotFound, "job not found")
			return
		}
		job.Origin = cur.Job.Origin
		job.AgentID = cur.Job.AgentID
		job.Version = cur.Job.Version + 1
	}
	if err := s.store.SaveJob(job); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	s.pushJobs(job.AgentID)
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	row, err := s.store.GetJob(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "job not found")
		return
	}
	if err := s.store.DeleteJob(row.Job.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	s.pushJobs(row.Job.AgentID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleJobRun is the GUI "Back up now" button: starts a backup on the
// remote agent immediately, or queues it if the agent is offline.
func (s *Server) handleJobRun(w http.ResponseWriter, r *http.Request) {
	req, _ := decodeBody[struct {
		Mode  string `json:"mode"`
		Queue bool   `json:"queue"`
	}](r)
	if req.Mode != "" && req.Mode != proto.ModeFull && req.Mode != proto.ModeIncremental {
		httpError(w, http.StatusBadRequest, "mode must be full or incremental")
		return
	}
	row, err := s.store.GetJob(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "job not found")
		return
	}
	runID, err := s.startBackup(row, req.Mode, req.Queue)
	if err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	queued := !s.hub.Online(row.Job.AgentID)
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "queued": queued})
}

// ---- runs ----

func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	runs, err := s.store.ListRuns(q.Get("agent"), q.Get("job"), limit)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, s.runsJSON(runs))
}

type runJSON struct {
	store.Run
	JobName   string `json:"job_name"`
	AgentName string `json:"agent_name"`
}

func (s *Server) runsJSON(runs []store.Run) []runJSON {
	agentNames := map[string]string{}
	if agents, err := s.store.ListAgents(); err == nil {
		for _, a := range agents {
			agentNames[a.ID] = a.Name
		}
	}
	jobNames := map[string]string{}
	if rows, err := s.store.ListJobs(""); err == nil {
		for _, row := range rows {
			jobNames[row.Job.ID] = row.Job.Name
		}
	}
	out := make([]runJSON, 0, len(runs))
	for _, run := range runs {
		name := jobNames[run.JobID]
		if name == "" {
			name = "(deleted job)"
		}
		out = append(out, runJSON{Run: run, JobName: name, AgentName: agentNames[run.AgentID]})
	}
	return out
}

func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "run not found")
		return
	}
	logs, err := s.store.RunLogs(run.ID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run":  s.runsJSON([]store.Run{*run})[0],
		"logs": logs,
	})
}

func (s *Server) handleRunCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.cancelRun(r.PathValue("id")); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- snapshots ----

func (s *Server) handleSnapshotsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	snaps, err := s.store.ListSnapshots(q.Get("agent"), q.Get("job"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, snaps)
}

func (s *Server) handleSnapshotTree(w http.ResponseWriter, r *http.Request) {
	sn, err := s.store.GetSnapshot(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	nodes, err := s.snapshotTree(sn, r.URL.Query().Get("path"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "manifest read failed")
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	sn, err := s.store.GetSnapshot(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	req, err := decodeBody[struct {
		Paths     []string `json:"paths"`
		TargetDir string   `json:"target_dir"`
		Overwrite bool     `json:"overwrite"`
	}](r)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad request body")
		return
	}
	runID, err := s.startRestore(sn, req.Paths, req.TargetDir, req.Overwrite)
	if err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"run_id": runID})
}

// handleSnapshotDownload streams a single file, or a zip of the selected
// paths when zip=1.
func (s *Server) handleSnapshotDownload(w http.ResponseWriter, r *http.Request) {
	sn, err := s.store.GetSnapshot(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	q := r.URL.Query()

	if q.Get("zip") == "1" {
		var prefixes []string
		if p := q.Get("paths"); p != "" {
			prefixes = strings.Split(p, "\x00")
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "snapshot-"+sn.ID+".zip"))
		if err := s.writeSnapshotZip(w, sn, prefixes); err != nil {
			log.Printf("zip download of snapshot %s: %v", sn.ID, err)
		}
		return
	}

	path := q.Get("path")
	var found *proto.ManifestEntry
	err = s.eachManifestEntry(sn.ManifestKey, func(e proto.ManifestEntry) error {
		if e.Type == "f" && strings.TrimLeft(e.Path, "/") == strings.TrimLeft(path, "/") {
			cp := e
			found = &cp
		}
		return nil
	})
	if err != nil || found == nil {
		httpError(w, http.StatusNotFound, "file not found in snapshot")
		return
	}
	name := found.Path[strings.LastIndexByte(found.Path, '/')+1:]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if err := s.streamFile(w, found.Hash); err != nil {
		log.Printf("file download from snapshot %s: %v", sn.ID, err)
	}
}

func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteSnapshot(r.PathValue("id")); err != nil {
		httpError(w, http.StatusInternalServerError, "snapshot delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- misc ----

func (s *Server) handleGC(w http.ResponseWriter, r *http.Request) {
	if err := s.PruneAndGC(); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	agents, err := s.agentsJSON()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	runs, err := s.store.ListRuns("", "", 15)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	stats, err := s.store.Stats()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": agents,
		"runs":   s.runsJSON(runs),
		"stats":  stats,
	})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	stats, _ := s.store.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"fingerprint": s.fingerprint,
		"stats":       stats,
		"username":    s.sessionUser(r).Username,
	})
}
