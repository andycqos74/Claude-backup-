package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"

	"centralbackup/internal/proto"
	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

// Background file push (server -> client). The admin uploads a file in the
// GUI; the payload is parked in the storage backend and the target agent
// fetches and writes it. The agent is a headless service, so the write is
// invisible on the client's screen — the whole operation is observed only
// from the server, as a "transfer" with live status. This is deliberately
// separate from backup runs so a push neither waits on nor blocks a backup.

// transferMaxBytes caps an uploaded file. Pushes are meant for configs,
// scripts and small installers, not bulk data (that is what jobs are for).
const transferMaxBytes = 2 << 30 // 2 GiB

// storeTransferPayload streams src into the backend at key, zstd-compressing
// on the way and returning the sha256 of the *raw* bytes plus the raw size.
// The hash lets the agent verify the file it writes is exactly what was
// uploaded.
func (s *Server) storeTransferPayload(key string, src io.Reader) (hash string, rawSize int64, err error) {
	pr, pw := io.Pipe()
	hasher := sha256.New()
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		enc, e := zstd.NewWriter(pw)
		if e != nil {
			pw.CloseWithError(e)
			done <- result{0, e}
			return
		}
		n, e := io.Copy(enc, io.TeeReader(src, hasher))
		if ce := enc.Close(); e == nil {
			e = ce
		}
		// A nil error closes the pipe with a clean EOF for the reader.
		pw.CloseWithError(e)
		done <- result{n, e}
	}()

	_, putErr := s.backend().Put(key, pr)
	res := <-done
	if putErr != nil {
		s.backend().Delete(key)
		return "", 0, putErr
	}
	if res.err != nil {
		s.backend().Delete(key)
		return "", 0, res.err
	}
	return hex.EncodeToString(hasher.Sum(nil)), res.n, nil
}

// dispatchTransfer sends a push command to the agent, marking the transfer
// failed if it cannot even be queued onto the connection.
func (s *Server) dispatchTransfer(t *store.Transfer) {
	ok := s.hub.Send(t.AgentID, proto.MsgPushFile, proto.PushFile{
		TransferID: t.ID, DestPath: t.DestPath, Filename: t.Filename,
		Hash: t.Hash, Size: t.Size, Mode: t.Mode, Overwrite: t.Overwrite,
	})
	if !ok {
		s.store.FinishTransfer(t.ID, proto.RunError, "", "failed to dispatch to client")
	}
}

// cancelTransfer aborts a queued or in-flight push. Queued pushes (nothing
// running yet) are cancelled server-side; a running one is cancelled by
// asking the agent, which reports back TransferDone{cancelled}.
func (s *Server) cancelTransfer(id string) error {
	t, err := s.store.GetTransfer(id)
	if err != nil {
		return err
	}
	switch t.Status {
	case proto.RunQueued:
		return s.store.FinishTransfer(id, proto.RunCancelled, "", "cancelled before it started")
	case proto.RunRunning:
		if !s.hub.Send(t.AgentID, proto.MsgCancelTransfer, proto.CancelTransfer{TransferID: id}) {
			return fmt.Errorf("client is offline; cannot reach it to cancel (it will be marked failed if it doesn't reconnect)")
		}
		return nil
	default:
		return fmt.Errorf("transfer is not in progress")
	}
}

// deleteTransfer removes the record and its stored payload.
func (s *Server) deleteTransfer(id string) error {
	key, err := s.store.DeleteTransfer(id)
	if err != nil {
		return err
	}
	if key != "" {
		s.backend().Delete(key)
	}
	return nil
}

// dispatchPendingTransfers fires queued pushes when an agent (re)connects.
func (s *Server) dispatchPendingTransfers(agentID string) {
	queued, err := s.store.QueuedTransfers(agentID)
	if err != nil {
		return
	}
	for _, t := range queued {
		s.store.MarkTransferStarted(t.ID)
		tt := t
		s.dispatchTransfer(&tt)
	}
}

func (s *Server) transferBelongsToAgent(id, agentID string) bool {
	t, err := s.store.GetTransfer(id)
	return err == nil && t.AgentID == agentID
}

// ---- admin API ----

// handleTransferCreate accepts a multipart upload (file + dest_path +
// overwrite) and pushes it to the client, or queues it if offline.
func (s *Server) handleTransferCreate(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if _, err := s.store.GetAgent(agentID); err != nil {
		httpError(w, http.StatusNotFound, "unknown client")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, transferMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		httpError(w, http.StatusRequestEntityTooLarge, "upload too large or malformed")
		return
	}
	defer r.MultipartForm.RemoveAll()

	dest := strings.TrimSpace(r.FormValue("dest_path"))
	if dest == "" {
		httpError(w, http.StatusBadRequest, "a destination path on the client is required")
		return
	}
	overwrite := r.FormValue("overwrite") == "1" || r.FormValue("overwrite") == "true"

	file, header, err := r.FormFile("file")
	if err != nil {
		httpError(w, http.StatusBadRequest, "a file to send is required")
		return
	}
	defer file.Close()

	// Keep only the base name of whatever the browser sent; the agent joins
	// it onto dest_path when that names a directory.
	filename := path.Base(strings.ReplaceAll(header.Filename, "\\", "/"))
	if filename == "." || filename == "/" || filename == "" {
		filename = "uploaded.bin"
	}

	id := store.NewID()
	key := storage.TransferKey(id)
	hash, size, err := s.storeTransferPayload(key, file)
	if err != nil {
		log.Printf("transfer upload for agent %s: %v", agentID, err)
		httpError(w, http.StatusInternalServerError, "could not store the uploaded file")
		return
	}

	online := s.hub.Online(agentID)
	status := proto.RunRunning
	if !online {
		status = proto.RunQueued
	}
	t := store.Transfer{
		ID: id, AgentID: agentID, Filename: filename, DestPath: dest,
		Size: size, Hash: hash, ObjectKey: key, Overwrite: overwrite, Status: status,
	}
	if err := s.store.CreateTransfer(t); err != nil {
		s.backend().Delete(key)
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if online {
		s.dispatchTransfer(&t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfer_id": id, "queued": !online})
}

func (s *Server) handleTransfersList(w http.ResponseWriter, r *http.Request) {
	transfers, err := s.store.ListTransfers(r.URL.Query().Get("agent"), 100)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, s.transfersJSON(transfers))
}

func (s *Server) handleTransferGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTransfer(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "transfer not found")
		return
	}
	writeJSON(w, http.StatusOK, s.transfersJSON([]store.Transfer{*t})[0])
}

func (s *Server) handleTransferCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.cancelTransfer(r.PathValue("id")); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTransferDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteTransfer(r.PathValue("id")); err != nil {
		httpError(w, http.StatusInternalServerError, "could not delete transfer")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type transferJSON struct {
	store.Transfer
	AgentName string `json:"agent_name"`
}

func (s *Server) transfersJSON(transfers []store.Transfer) []transferJSON {
	names := map[string]string{}
	if agents, err := s.store.ListAgents(); err == nil {
		for _, a := range agents {
			names[a.ID] = a.Name
		}
	}
	out := make([]transferJSON, 0, len(transfers))
	for _, t := range transfers {
		out = append(out, transferJSON{Transfer: t, AgentName: names[t.AgentID]})
	}
	return out
}

// ---- agent data plane ----

// handleTransferContent streams a pushed file's (compressed) payload to the
// agent it was addressed to.
func (s *Server) handleTransferContent(w http.ResponseWriter, r *http.Request, agentID string) {
	t, err := s.store.GetTransfer(r.PathValue("id"))
	if err != nil || t.AgentID != agentID {
		httpError(w, http.StatusNotFound, "transfer not found")
		return
	}
	rc, err := s.backend().Get(t.ObjectKey)
	if err != nil {
		httpError(w, http.StatusNotFound, "payload not found")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/zstd")
	io.Copy(w, rc)
}
