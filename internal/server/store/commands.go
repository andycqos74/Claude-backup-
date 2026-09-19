package store

import (
	"database/sql"

	"centralbackup/internal/proto"
)

// Command is one remote shell command run on a client: what was run, in
// which shell, and how it ended. Its output lines live in command_output,
// keyed by a monotonically increasing seq so the GUI can poll incrementally.
type Command struct {
	ID         string
	AgentID    string
	Shell      string
	Command    string
	Status     string
	ExitCode   int
	Error      string
	CreatedAt  int64
	StartedAt  int64
	FinishedAt int64
}

const commandCols = `id, agent_id, shell, command, status, exit_code, error, created_at, started_at, finished_at`

func scanCommand(scan func(dest ...any) error) (*Command, error) {
	var c Command
	err := scan(&c.ID, &c.AgentID, &c.Shell, &c.Command, &c.Status, &c.ExitCode, &c.Error,
		&c.CreatedAt, &c.StartedAt, &c.FinishedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateCommand records a command that is being dispatched (status running).
func (s *Store) CreateCommand(id, agentID, shell, command string) error {
	_, err := s.db.Exec(`INSERT INTO commands (id, agent_id, shell, command, status, created_at, started_at)
		VALUES (?,?,?,?,?,?,?)`, id, agentID, shell, command, proto.RunRunning, now(), now())
	return err
}

func (s *Store) GetCommand(id string) (*Command, error) {
	return scanCommand(s.db.QueryRow(`SELECT `+commandCols+` FROM commands WHERE id = ?`, id).Scan)
}

func (s *Store) ListCommands(agentID string, limit int) ([]Command, error) {
	q := `SELECT ` + commandCols + ` FROM commands`
	var args []any
	if agentID != "" {
		q += ` WHERE agent_id = ?`
		args = append(args, agentID)
	}
	q += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		c, err := scanCommand(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// AppendCommandOutput stores one output line and returns its seq.
func (s *Store) AppendCommandOutput(commandID, stream, line string) error {
	_, err := s.db.Exec(`INSERT INTO command_output (command_id, ts, stream, line) VALUES (?,?,?,?)`,
		commandID, now(), stream, line)
	return err
}

// CommandLine is one line of captured output.
type CommandLine struct {
	Seq    int64  `json:"seq"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// CommandOutput returns output lines with seq greater than afterSeq, so the
// GUI can poll for just what is new.
func (s *Store) CommandOutput(commandID string, afterSeq int64, limit int) ([]CommandLine, error) {
	rows, err := s.db.Query(`SELECT seq, stream, line FROM command_output
		WHERE command_id = ? AND seq > ? ORDER BY seq LIMIT ?`, commandID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommandLine
	for rows.Next() {
		var l CommandLine
		if err := rows.Scan(&l.Seq, &l.Stream, &l.Line); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) FinishCommand(id, status string, exitCode int, errMsg string) error {
	_, err := s.db.Exec(`UPDATE commands SET status = ?, exit_code = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, exitCode, errMsg, now(), id)
	return err
}

// FailRunningCommands marks an agent's in-flight commands failed when it
// disconnects (their output can no longer arrive).
func (s *Store) FailRunningCommands(agentID, reason string) error {
	_, err := s.db.Exec(`UPDATE commands SET status = ?, error = ?, finished_at = ? WHERE agent_id = ? AND status = ?`,
		proto.RunError, reason, now(), agentID, proto.RunRunning)
	return err
}

// DeleteCommand removes a command and its output.
func (s *Store) DeleteCommand(id string) error {
	if _, err := s.db.Exec(`DELETE FROM command_output WHERE command_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM commands WHERE id = ?`, id)
	return err
}
