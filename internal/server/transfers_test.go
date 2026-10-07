package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"github.com/klauspost/compress/zstd"

	"centralbackup/internal/proto"
	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

// decompress reads a whole zstd stream into memory.
func decompress(t *testing.T, r io.Reader) []byte {
	t.Helper()
	dec, err := zstd.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	b, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestStoreTransferPayloadRoundTrip stores a payload, then reads it back the
// way the agent's content endpoint serves it, checking the returned hash and
// size describe the raw bytes.
func TestStoreTransferPayloadRoundTrip(t *testing.T) {
	s := testServer(t)
	content := bytes.Repeat([]byte("central-backup file push\n"), 1000)
	key := storage.TransferKey("tp1")

	hash, size, err := s.storeTransferPayload(key, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) {
		t.Errorf("raw size = %d, want %d", size, len(content))
	}
	want := sha256.Sum256(content)
	if hash != hex.EncodeToString(want[:]) {
		t.Errorf("hash mismatch")
	}

	// The object is compressed at rest; decompressing it yields the original.
	rc, err := s.backend().Get(key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if got := decompress(t, rc); !bytes.Equal(got, content) {
		t.Errorf("stored payload does not round-trip")
	}
}

func TestTransferContentEndpointAuth(t *testing.T) {
	s := testServer(t)
	mine, _, _ := s.store.CreateAgent("mine", proto.EnrollRequest{Hostname: "h"})
	other, _, _ := s.store.CreateAgent("other", proto.EnrollRequest{Hostname: "h2"})

	content := []byte("secret config")
	key := storage.TransferKey("c1")
	hash, size, _ := s.storeTransferPayload(key, bytes.NewReader(content))
	s.store.CreateTransfer(store.Transfer{
		ID: "c1", AgentID: mine, Filename: "config", DestPath: "/x",
		Hash: hash, Size: size, ObjectKey: key, Status: proto.RunRunning,
	})

	// The addressed agent gets the payload back, and it decompresses to the
	// original.
	r := httptest.NewRequest("GET", "/api/agent/transfers/c1/content", nil)
	r.SetPathValue("id", "c1")
	w := httptest.NewRecorder()
	s.handleTransferContent(w, r, mine)
	if w.Code != 200 {
		t.Fatalf("addressed agent got %d", w.Code)
	}
	if got := decompress(t, w.Body); !bytes.Equal(got, content) {
		t.Errorf("payload mismatch")
	}

	// A different agent must not be able to fetch someone else's payload.
	r2 := httptest.NewRequest("GET", "/api/agent/transfers/c1/content", nil)
	r2.SetPathValue("id", "c1")
	w2 := httptest.NewRecorder()
	s.handleTransferContent(w2, r2, other)
	if w2.Code != 404 {
		t.Errorf("foreign agent got %d, want 404", w2.Code)
	}
}

// TestTransferUploadQueuesWhenOffline drives the admin multipart upload path
// for an offline client: the file is stored and a queued transfer is
// recorded with the right hash and size.
func TestTransferUploadQueuesWhenOffline(t *testing.T) {
	s := testServer(t)
	agentID, _, _ := s.store.CreateAgent("box", proto.EnrollRequest{Hostname: "box"})

	content := []byte("#!/bin/sh\necho hello\n")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("dest_path", "/opt/app/run.sh")
	mw.WriteField("overwrite", "1")
	fw, _ := mw.CreateFormFile("file", "run.sh")
	fw.Write(content)
	mw.Close()

	r := httptest.NewRequest("POST", "/api/admin/agents/"+agentID+"/transfers", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	s.handleTransferCreate(w, r)

	if w.Code != 200 {
		t.Fatalf("upload status %d: %s", w.Code, w.Body.String())
	}

	transfers, err := s.store.ListTransfers(agentID, 10)
	if err != nil || len(transfers) != 1 {
		t.Fatalf("expected 1 transfer, got %d (%v)", len(transfers), err)
	}
	tr := transfers[0]
	// Agent is offline in this test, so the push waits for reconnect.
	if tr.Status != proto.RunQueued {
		t.Errorf("status = %q, want queued", tr.Status)
	}
	if tr.Filename != "run.sh" || tr.DestPath != "/opt/app/run.sh" || !tr.Overwrite {
		t.Errorf("transfer fields wrong: %+v", tr)
	}
	want := sha256.Sum256(content)
	if tr.Hash != hex.EncodeToString(want[:]) || tr.Size != int64(len(content)) {
		t.Errorf("hash/size wrong: %+v", tr)
	}
	// The payload really landed in the backend.
	if ok, _ := s.backend().Has(tr.ObjectKey); !ok {
		t.Errorf("payload not stored at %s", tr.ObjectKey)
	}
}

// TestPullRoundTrip drives the full client->server pull path from the
// server's side: create the pull, receive the agent's upload, and serve the
// admin download — all reconstructing the original bytes.
func TestPullRoundTrip(t *testing.T) {
	s := testServer(t)
	agentID, _, _ := s.store.CreateAgent("box", proto.EnrollRequest{Hostname: "box"})

	// Admin requests a file off the client (offline here, so it queues).
	r := httptest.NewRequest("POST", "/api/admin/agents/"+agentID+"/pull",
		bytes.NewReader([]byte(`{"source_path":"/var/log/app/current.log"}`)))
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	s.handleTransferPull(w, r)
	if w.Code != 200 {
		t.Fatalf("pull create status %d: %s", w.Code, w.Body.String())
	}
	transfers, _ := s.store.ListTransfers(agentID, 10)
	if len(transfers) != 1 {
		t.Fatalf("expected 1 transfer, got %d", len(transfers))
	}
	tr := transfers[0]
	if tr.Direction != proto.DirectionPull || tr.Status != proto.RunQueued {
		t.Errorf("pull transfer wrong: %+v", tr)
	}
	if tr.Filename != "current.log" || tr.SourcePath != "/var/log/app/current.log" {
		t.Errorf("pull fields wrong: %+v", tr)
	}

	// The agent uploads the file's (compressed) content.
	content := bytes.Repeat([]byte("log line\n"), 500)
	var comp bytes.Buffer
	enc, _ := zstd.NewWriter(&comp)
	enc.Write(content)
	enc.Close()
	up := httptest.NewRequest("PUT", "/api/agent/transfers/"+tr.ID+"/content", bytes.NewReader(comp.Bytes()))
	up.SetPathValue("id", tr.ID)
	uw := httptest.NewRecorder()
	s.handleTransferUpload(uw, up, agentID)
	if uw.Code != 200 {
		t.Fatalf("upload status %d: %s", uw.Code, uw.Body.String())
	}

	// The server recorded the raw size and hash of the uploaded file.
	got, _ := s.store.GetTransfer(tr.ID)
	want := sha256.Sum256(content)
	if got.Hash != hex.EncodeToString(want[:]) || got.Size != int64(len(content)) {
		t.Errorf("payload not recorded: %+v", got)
	}
	// Mark success (the agent's transfer_done would do this over the socket).
	s.store.FinishTransfer(tr.ID, proto.RunSuccess, "", "")

	// Admin downloads it — decompressed, original bytes, right filename.
	dr := httptest.NewRequest("GET", "/api/admin/transfers/"+tr.ID+"/download", nil)
	dr.SetPathValue("id", tr.ID)
	dw := httptest.NewRecorder()
	s.handleTransferDownload(dw, dr)
	if dw.Code != 200 {
		t.Fatalf("download status %d", dw.Code)
	}
	if !bytes.Equal(dw.Body.Bytes(), content) {
		t.Errorf("downloaded content does not match original")
	}
	if cd := dw.Header().Get("Content-Disposition"); cd != `attachment; filename="current.log"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestUploadRejectsPushTransfer(t *testing.T) {
	s := testServer(t)
	agentID, _, _ := s.store.CreateAgent("box", proto.EnrollRequest{Hostname: "box"})
	// A push transfer must not accept an agent upload.
	s.store.CreateTransfer(store.Transfer{
		ID: "p1", AgentID: agentID, Direction: proto.DirectionPush,
		Filename: "f", DestPath: "/x", ObjectKey: "k", Status: proto.RunRunning,
	})
	up := httptest.NewRequest("PUT", "/api/agent/transfers/p1/content", bytes.NewReader([]byte("x")))
	up.SetPathValue("id", "p1")
	uw := httptest.NewRecorder()
	s.handleTransferUpload(uw, up, agentID)
	if uw.Code != 400 {
		t.Errorf("push upload status %d, want 400", uw.Code)
	}
}

func TestDownloadOnlySucceededPulls(t *testing.T) {
	s := testServer(t)
	agentID, _, _ := s.store.CreateAgent("box", proto.EnrollRequest{Hostname: "box"})
	s.store.CreateTransfer(store.Transfer{
		ID: "d1", AgentID: agentID, Direction: proto.DirectionPull,
		Filename: "f", SourcePath: "/x", ObjectKey: "k", Status: proto.RunRunning,
	})
	dr := httptest.NewRequest("GET", "/api/admin/transfers/d1/download", nil)
	dr.SetPathValue("id", "d1")
	dw := httptest.NewRecorder()
	s.handleTransferDownload(dw, dr)
	if dw.Code != 409 {
		t.Errorf("download of in-flight pull status %d, want 409", dw.Code)
	}
}

func TestTransferUploadRejectsMissingFields(t *testing.T) {
	s := testServer(t)
	agentID, _, _ := s.store.CreateAgent("box", proto.EnrollRequest{Hostname: "box"})

	// No dest_path.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "f")
	fw.Write([]byte("x"))
	mw.Close()
	r := httptest.NewRequest("POST", "/api/admin/agents/"+agentID+"/transfers", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.SetPathValue("id", agentID)
	w := httptest.NewRecorder()
	s.handleTransferCreate(w, r)
	if w.Code != 400 {
		t.Errorf("missing dest_path: status %d, want 400", w.Code)
	}

	// Unknown agent.
	var body2 bytes.Buffer
	mw2 := multipart.NewWriter(&body2)
	mw2.WriteField("dest_path", "/x")
	fw2, _ := mw2.CreateFormFile("file", "f")
	fw2.Write([]byte("x"))
	mw2.Close()
	r2 := httptest.NewRequest("POST", "/api/admin/agents/nope/transfers", &body2)
	r2.Header.Set("Content-Type", mw2.FormDataContentType())
	r2.SetPathValue("id", "nope")
	w2 := httptest.NewRecorder()
	s.handleTransferCreate(w2, r2)
	if w2.Code != 404 {
		t.Errorf("unknown agent: status %d, want 404", w2.Code)
	}
}
