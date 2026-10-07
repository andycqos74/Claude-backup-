package store

import (
	"testing"

	"centralbackup/internal/proto"
)

func TestCommandLifecycle(t *testing.T) {
	s := openTest(t)
	if err := s.CreateCommand("c1", "a1", proto.ShellPowerShell, "Get-Service"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCommand("c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != proto.RunRunning || got.StartedAt == 0 || got.Shell != proto.ShellPowerShell {
		t.Fatalf("new command wrong: %+v", got)
	}

	// Output is captured with monotonic seq and fetched incrementally.
	s.AppendCommandOutput("c1", "stdout", "line 1")
	s.AppendCommandOutput("c1", "stderr", "a warning")
	s.AppendCommandOutput("c1", "stdout", "line 3")

	all, err := s.CommandOutput("c1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[1].Stream != "stderr" || all[0].Line != "line 1" {
		t.Fatalf("output wrong: %+v", all)
	}
	// Only lines after a given seq.
	rest, _ := s.CommandOutput("c1", all[0].Seq, 100)
	if len(rest) != 2 || rest[0].Line != "a warning" {
		t.Errorf("incremental fetch wrong: %+v", rest)
	}

	if err := s.FinishCommand("c1", proto.RunError, 5, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetCommand("c1")
	if got.Status != proto.RunError || got.ExitCode != 5 || got.FinishedAt == 0 {
		t.Errorf("finished command wrong: %+v", got)
	}
}

func TestFailRunningCommands(t *testing.T) {
	s := openTest(t)
	s.CreateCommand("r1", "a1", proto.ShellSh, "sleep 100")
	s.FinishCommand("done", proto.RunSuccess, 0, "") // no-op (missing) — just exercises path
	if err := s.FailRunningCommands("a1", "client disconnected"); err != nil {
		t.Fatal(err)
	}
	r1, _ := s.GetCommand("r1")
	if r1.Status != proto.RunError || r1.Error == "" {
		t.Errorf("running command should be failed on disconnect: %+v", r1)
	}
}

func TestDeleteCommandRemovesOutput(t *testing.T) {
	s := openTest(t)
	s.CreateCommand("d1", "a1", proto.ShellSh, "echo hi")
	s.AppendCommandOutput("d1", "stdout", "hi")
	if err := s.DeleteCommand("d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCommand("d1"); err != ErrNotFound {
		t.Errorf("command should be gone: %v", err)
	}
	lines, _ := s.CommandOutput("d1", 0, 100)
	if len(lines) != 0 {
		t.Errorf("output should be gone, got %d lines", len(lines))
	}
}
