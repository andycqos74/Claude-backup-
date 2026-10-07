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

// dispatchTransfer sends the push or pull command to the agent, marking the
// transfer failed if it cannot even be queued onto the connection.
func (s *Server) dispatchTransfer(t *store.Transfer) {
	var ok bool
	if t.Direction == proto.DirectionPull {
		ok = s.hub.Send(t.AgentID, proto.MsgPullFile, proto.PullFile{
			TransferID: t.ID, SourcePath: t.SourcePath,
		})
	} else {
		ok = s.hub.Send(t.AgentID, proto.MsgPushFile, proto.PushFile{
			TransferID: t.ID, DestPath: t.DestPath, Filename: t.Filename,
			Hash: t.Hash, Size: t.Size, Mode: t.Mode, Overwrite: t.Overwrite,
		})
	}
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

// handleTransferPull starts a client->server file fetch: the admin names a
// path on the client, and the agent uploads that file to the server where it
// can be downloaded.
func (s *Server) handleTransferPull(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if _, err := s.store.GetAgent(agentID); err != nil {
		httpError(w, http.StatusNotFound, "unknown client")
		return
	}
	req, err := decodeBody[struct {
		SourcePath string `json:"source_path"`
	}](r)
	if err != nil || strings.TrimSpace(req.SourcePath) == "" {
		httpError(w, http.StatusBadRequest, "a source path on the client is required")
		return
	}
	src := strings.TrimSpace(req.SourcePath)
	filename := path.Base(strings.ReplaceAll(src, "\\", "/"))
	if filename == "." || filename == "/" || filename == "" {
		httpError(w, http.StatusBadRequest, "source path does not name a file")
		return
	}

	id := store.NewID()
	online := s.hub.Online(agentID)
	status := proto.RunRunning
	if !online {
		status = proto.RunQueued
	}
	t := store.Transfer{
		ID: id, AgentID: agentID, Direction: proto.DirectionPull,
		Filename: filename, SourcePath: src, ObjectKey: storage.TransferKey(id), Status: status,
	}
	if err := s.store.CreateTransfer(t); err != nil {
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

// handleTransferDownload streams a pulled file back to the admin's browser,
// decompressing it on the way.
func (s *Server) handleTransferDownload(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTransfer(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "transfer not found")
		return
	}
	if t.Direction != proto.DirectionPull || t.Status != proto.RunSuccess {
		httpError(w, http.StatusConflict, "no downloadable file for this transfer")
		return
	}
	rc, err := s.backend().Get(t.ObjectKey)
	if err != nil {
		httpError(w, http.StatusNotFound, "payload not found")
		return
	}
	defer rc.Close()
	dec, err := zstd.NewReader(rc)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "decompress failed")
		return
	}
	defer dec.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", t.Filename))
	io.Copy(w, dec.IOReadCloser())
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

// handleTransferUpload ingests the file an agent read off its client for a
// pull. The body is the zstd-compressed content; the server stores it and
// records the raw size and hash on the transfer (verified by decompressing a
// copy on the fly, exactly like a blob upload).
func (s *Server) handleTransferUpload(w http.ResponseWriter, r *http.Request, agentID string) {
	t, err := s.store.GetTransfer(r.PathValue("id"))
	if err != nil || t.AgentID != agentID {
		httpError(w, http.StatusNotFound, "transfer not found")
		return
	}
	if t.Direction != proto.DirectionPull {
		httpError(w, http.StatusBadRequest, "not a pull transfer")
		return
	}

	pr, pw := io.Pipe()
	hasher := sha256.New()
	var rawSize int64
	verify := make(chan error, 1)
	go func() {
		dec, err := zstd.NewReader(pr)
		if err != nil {
			pr.CloseWithError(err)
			verify <- err
			return
		}
		defer dec.Close()
		n, err := io.Copy(hasher, dec.IOReadCloser())
		rawSize = n
		io.Copy(io.Discard, pr)
		verify <- err
	}()

	_, putErr := s.backend().Put(t.ObjectKey, io.TeeReader(io.LimitReader(r.Body, transferMaxBytes+(1<<20)), pw))
	pw.Close()
	decErr := <-verify

	if putErr != nil || decErr != nil {
		s.backend().Delete(t.ObjectKey)
		httpError(w, http.StatusBadRequest, "file upload failed")
		return
	}
	if err := s.store.SetTransferPayload(t.ID, hex.EncodeToString(hasher.Sum(nil)), rawSize); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

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
