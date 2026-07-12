package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"

	"centralbackup/internal/proto"
	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// agentAuth authenticates data-plane requests with the per-agent id+secret
// issued at enrollment.
func (s *Server) agentAuth(next func(w http.ResponseWriter, r *http.Request, agentID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Agent-Id")
		key := r.Header.Get("X-Agent-Key")
		if id == "" || !s.store.VerifyAgent(id, key) {
			httpError(w, http.StatusUnauthorized, "invalid agent credentials")
			return
		}
		next(w, r, id)
	}
}

// handleEnroll exchanges a one-time enrollment token for permanent agent
// credentials.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req proto.EnrollRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request body")
		return
	}
	id, secret, err := s.store.CreateAgent(req.Name, req)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "create agent failed")
		return
	}
	if err := s.store.ConsumeEnrollToken(req.Token, id); err != nil {
		s.store.DeleteAgent(id)
		httpError(w, http.StatusForbidden, "invalid or expired enrollment token")
		return
	}
	log.Printf("agent enrolled: %s (%s, %s/%s)", id, req.Hostname, req.OS, req.Arch)
	writeJSON(w, http.StatusOK, proto.EnrollResponse{AgentID: id, Secret: secret})
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	// Agents are not browsers; no Origin to check.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// handleAgentWS is the persistent control channel. The agent authenticates
// on the upgrade request; afterwards the server can push commands at any
// time and the agent streams run events back.
func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	agentID := r.Header.Get("X-Agent-Id")
	if agentID == "" || !s.store.VerifyAgent(agentID, r.Header.Get("X-Agent-Key")) {
		httpError(w, http.StatusUnauthorized, "invalid agent credentials")
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn := s.hub.register(agentID, ws)
	defer func() {
		s.hub.unregister(conn)
		s.store.FailRunningRuns(agentID, "agent disconnected")
		log.Printf("agent disconnected: %s", agentID)
	}()

	ws.SetReadLimit(16 << 20)
	resetDeadline := func() { ws.SetReadDeadline(time.Now().Add(90 * time.Second)) }
	resetDeadline()
	ws.SetPongHandler(func(string) error { resetDeadline(); return nil })

	log.Printf("agent connected: %s", agentID)
	for {
		var env proto.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			return
		}
		resetDeadline()
		if err := s.handleAgentMessage(agentID, env); err != nil {
			log.Printf("agent %s: %s message: %v", agentID, env.Type, err)
		}
	}
}

func (s *Server) handleAgentMessage(agentID string, env proto.Envelope) error {
	switch env.Type {
	case proto.MsgHello:
		hello, err := unmarshal[proto.Hello](env.Data)
		if err != nil {
			return err
		}
		if err := s.store.TouchAgent(agentID, &hello); err != nil {
			return err
		}
		s.pushJobs(agentID)
		go s.dispatchPendingWork(agentID)
		return nil

	case proto.MsgJobsSync:
		sync, err := unmarshal[proto.JobsSync](env.Data)
		if err != nil {
			return err
		}
		return s.handleJobsSync(agentID, sync)

	case proto.MsgJobDelete:
		del, err := unmarshal[proto.JobDelete](env.Data)
		if err != nil {
			return err
		}
		return s.handleAgentJobDelete(agentID, del.JobID)

	case proto.MsgRunProgress:
		p, err := unmarshal[proto.RunProgress](env.Data)
		if err != nil {
			return err
		}
		if !s.runBelongsToAgent(p.RunID, agentID) {
			return nil
		}
		return s.store.UpdateRunProgress(p.RunID, p)

	case proto.MsgRunLog:
		l, err := unmarshal[proto.RunLog](env.Data)
		if err != nil {
			return err
		}
		if !s.runBelongsToAgent(l.RunID, agentID) {
			return nil
		}
		return s.store.AppendRunLog(l.RunID, l.Level, l.Message)

	case proto.MsgRunDone:
		d, err := unmarshal[proto.RunDone](env.Data)
		if err != nil {
			return err
		}
		if !s.runBelongsToAgent(d.RunID, agentID) {
			return nil
		}
		s.store.TouchAgent(agentID, nil)
		return s.store.FinishRun(d.RunID, d.Status, d.SnapshotID, d.Error, d.Stats)

	default:
		log.Printf("agent %s: unknown message type %q", agentID, env.Type)
		return nil
	}
}

func (s *Server) runBelongsToAgent(runID, agentID string) bool {
	run, err := s.store.GetRun(runID)
	return err == nil && run.AgentID == agentID
}

// ---- data plane ----

func (s *Server) handleBlobCheck(w http.ResponseWriter, r *http.Request, agentID string) {
	var req proto.BlobCheckRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request body")
		return
	}
	for _, h := range req.Hashes {
		if !hashRe.MatchString(h) {
			httpError(w, http.StatusBadRequest, "invalid hash")
			return
		}
	}
	missing, err := s.store.MissingBlobs(s.backendKey(), req.Hashes)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, proto.BlobCheckResponse{Missing: missing})
}

// handleBlobPut ingests one zstd-compressed blob. The server decompresses
// on the fly to verify the content hash matches the claimed hash before the
// blob becomes visible in the index.
func (s *Server) handleBlobPut(w http.ResponseWriter, r *http.Request, agentID string) {
	hash := r.PathValue("hash")
	if !hashRe.MatchString(hash) {
		httpError(w, http.StatusBadRequest, "invalid hash")
		return
	}
	if ok, _ := s.store.HasBlob(s.backendKey(), hash); ok {
		io.Copy(io.Discard, r.Body)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	// Tee the compressed stream into storage while decompressing a copy to
	// verify the hash.
	pr, pw := io.Pipe()
	hasher := sha256.New()
	var rawSize int64
	verifyErr := make(chan error, 1)
	go func() {
		dec, err := zstd.NewReader(pr)
		if err != nil {
			pr.CloseWithError(err)
			verifyErr <- err
			return
		}
		defer dec.Close()
		n, err := io.Copy(hasher, dec.IOReadCloser())
		rawSize = n
		// Drain any trailing bytes so the tee never blocks.
		io.Copy(io.Discard, pr)
		verifyErr <- err
	}()

	key := storage.BlobKey(hash)
	stored, putErr := s.backend().Put(key, io.TeeReader(r.Body, pw))
	pw.Close()
	decErr := <-verifyErr

	if putErr != nil || decErr != nil {
		s.backend().Delete(key)
		httpError(w, http.StatusBadRequest, "blob upload failed")
		return
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != hash {
		s.backend().Delete(key)
		httpError(w, http.StatusBadRequest, "content hash mismatch")
		return
	}
	if err := s.store.AddBlob(s.backendKey(), hash, rawSize, stored); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleBlobGet(w http.ResponseWriter, r *http.Request, agentID string) {
	hash := r.PathValue("hash")
	if !hashRe.MatchString(hash) {
		httpError(w, http.StatusBadRequest, "invalid hash")
		return
	}
	rc, err := s.backend().Get(storage.BlobKey(hash))
	if err != nil {
		httpError(w, http.StatusNotFound, "blob not found")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/zstd")
	io.Copy(w, rc)
}

// handleSnapshotCommit stores the manifest and records the snapshot. Body is
// the zstd-compressed JSONL manifest.
func (s *Server) handleSnapshotCommit(w http.ResponseWriter, r *http.Request, agentID string) {
	q := r.URL.Query()
	jobID, runID, mode := q.Get("job_id"), q.Get("run_id"), q.Get("mode")
	files := parseInt64(q.Get("files"))
	bytes := parseInt64(q.Get("bytes"))

	job, err := s.store.GetJob(jobID)
	if err != nil || job.Job.AgentID != agentID {
		httpError(w, http.StatusForbidden, "unknown job")
		return
	}
	if runID != "" && !s.runBelongsToAgent(runID, agentID) {
		httpError(w, http.StatusForbidden, "unknown run")
		return
	}

	// Hold the commit read-lock so GC never sweeps blobs referenced by a
	// manifest that is mid-commit.
	s.commitMu.RLock()
	defer s.commitMu.RUnlock()

	snapID := store.NewID()
	key := storage.ManifestKey(snapID)
	if _, err := s.backend().Put(key, io.LimitReader(r.Body, 8<<30)); err != nil {
		httpError(w, http.StatusInternalServerError, "manifest store failed")
		return
	}
	err = s.store.CreateSnapshot(store.Snapshot{
		ID: snapID, Backend: s.backendKey(), JobID: jobID, AgentID: agentID, RunID: runID,
		Mode: mode, Files: files, Bytes: bytes, ManifestKey: key,
	})
	if err != nil {
		s.backend().Delete(key)
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, proto.SnapshotCommitResponse{SnapshotID: snapID})
}

func (s *Server) handleManifestGet(w http.ResponseWriter, r *http.Request, agentID string) {
	sn, err := s.store.GetSnapshot(r.PathValue("id"))
	if err != nil || sn.AgentID != agentID {
		httpError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	rc, err := s.backend().Get(sn.ManifestKey)
	if err != nil {
		httpError(w, http.StatusNotFound, "manifest not found")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/zstd")
	io.Copy(w, rc)
}

func parseInt64(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
