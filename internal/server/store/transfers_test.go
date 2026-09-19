package store

import (
	"testing"

	"centralbackup/internal/proto"
)

func TestTransferLifecycle(t *testing.T) {
	s := openTest(t)

	// A push to an online client starts running; progress and completion
	// are recorded.
	err := s.CreateTransfer(Transfer{
		ID: "t1", AgentID: "a1", Filename: "config.yaml", DestPath: "/etc/app/",
		Size: 100, Hash: "abc", ObjectKey: "transfers/t1.zst", Overwrite: true,
		Status: proto.RunRunning,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTransfer("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != proto.RunRunning || got.StartedAt == 0 {
		t.Errorf("running transfer should have a start time: %+v", got)
	}
	if !got.Overwrite || got.Filename != "config.yaml" || got.Size != 100 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	if err := s.UpdateTransferProgress("t1", 42); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTransfer("t1", proto.RunSuccess, "/etc/app/config.yaml", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTransfer("t1")
	if got.Status != proto.RunSuccess || got.BytesDone != 42 || got.FinishedAt == 0 {
		t.Errorf("finished transfer wrong: %+v", got)
	}
	if got.WrittenPath != "/etc/app/config.yaml" {
		t.Errorf("written path not recorded: %q", got.WrittenPath)
	}
}

func TestTransferQueueAndDispatch(t *testing.T) {
	s := openTest(t)

	// Offline client: queued, appears in QueuedTransfers, then starts.
	if err := s.CreateTransfer(Transfer{
		ID: "q1", AgentID: "a1", Filename: "f", DestPath: "/tmp/f",
		ObjectKey: "transfers/q1.zst", Status: proto.RunQueued,
	}); err != nil {
		t.Fatal(err)
	}
	// A different agent's queued transfer must not leak in.
	s.CreateTransfer(Transfer{ID: "q2", AgentID: "a2", Filename: "f", DestPath: "/tmp/f",
		ObjectKey: "transfers/q2.zst", Status: proto.RunQueued})

	q, err := s.QueuedTransfers("a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 1 || q[0].ID != "q1" {
		t.Fatalf("expected only q1 queued for a1, got %+v", q)
	}
	if q[0].StartedAt != 0 {
		t.Errorf("queued transfer should not have a start time")
	}

	if err := s.MarkTransferStarted("q1"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTransfer("q1")
	if got.Status != proto.RunRunning || got.StartedAt == 0 {
		t.Errorf("after start: %+v", got)
	}
	if q, _ := s.QueuedTransfers("a1"); len(q) != 0 {
		t.Errorf("q1 should no longer be queued")
	}
}

func TestFailRunningTransfers(t *testing.T) {
	s := openTest(t)
	s.CreateTransfer(Transfer{ID: "r1", AgentID: "a1", Filename: "f", DestPath: "/x",
		ObjectKey: "k1", Status: proto.RunRunning})
	s.CreateTransfer(Transfer{ID: "r2", AgentID: "a1", Filename: "f", DestPath: "/x",
		ObjectKey: "k2", Status: proto.RunQueued})

	if err := s.FailRunningTransfers("a1", "client disconnected"); err != nil {
		t.Fatal(err)
	}
	r1, _ := s.GetTransfer("r1")
	if r1.Status != proto.RunError || r1.Error == "" {
		t.Errorf("running transfer should be failed on disconnect: %+v", r1)
	}
	// Queued survives a disconnect so it can dispatch on reconnect.
	r2, _ := s.GetTransfer("r2")
	if r2.Status != proto.RunQueued {
		t.Errorf("queued transfer should survive disconnect: %+v", r2)
	}
}

func TestDeleteTransferReturnsKey(t *testing.T) {
	s := openTest(t)
	s.CreateTransfer(Transfer{ID: "d1", AgentID: "a1", Filename: "f", DestPath: "/x",
		ObjectKey: "transfers/d1.zst", Status: proto.RunSuccess})

	key, err := s.DeleteTransfer("d1")
	if err != nil {
		t.Fatal(err)
	}
	if key != "transfers/d1.zst" {
		t.Errorf("DeleteTransfer returned key %q", key)
	}
	if _, err := s.GetTransfer("d1"); err != ErrNotFound {
		t.Errorf("transfer should be gone, got %v", err)
	}
}

func TestTransferKeysForAgent(t *testing.T) {
	s := openTest(t)
	s.CreateTransfer(Transfer{ID: "k1", AgentID: "a1", Filename: "f", DestPath: "/x",
		ObjectKey: "transfers/k1.zst", Status: proto.RunSuccess})
	s.CreateTransfer(Transfer{ID: "k2", AgentID: "a1", Filename: "f", DestPath: "/x",
		ObjectKey: "transfers/k2.zst", Status: proto.RunQueued})
	s.CreateTransfer(Transfer{ID: "k3", AgentID: "a2", Filename: "f", DestPath: "/x",
		ObjectKey: "transfers/k3.zst", Status: proto.RunSuccess})

	keys, err := s.TransferKeysForAgent("a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys for a1, got %v", keys)
	}
}
