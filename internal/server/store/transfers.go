package store

import (
	"database/sql"

	"centralbackup/internal/proto"
)

// Transfer is one background file push from the server to a client: an admin
// uploads a file, the payload is parked in the storage backend under
// ObjectKey, and the agent fetches and writes it. Status uses the same
// terminal values as runs (queued | running | success | error | cancelled).
type Transfer struct {
	ID          string
	AgentID     string
	Filename    string
	DestPath    string
	Size        int64
	Hash        string
	ObjectKey   string
	Mode        uint32
	Overwrite   bool
	Status      string
	Error       string
	WrittenPath string
	BytesDone   int64
	CreatedAt   int64
	StartedAt   int64
	FinishedAt  int64
}

const transferCols = `id, agent_id, filename, dest_path, size, hash, object_key, mode, overwrite,
	status, error, written_path, bytes_done, created_at, started_at, finished_at`

func scanTransfer(scan func(dest ...any) error) (*Transfer, error) {
	var t Transfer
	var overwrite int
	err := scan(&t.ID, &t.AgentID, &t.Filename, &t.DestPath, &t.Size, &t.Hash, &t.ObjectKey,
		&t.Mode, &overwrite, &t.Status, &t.Error, &t.WrittenPath, &t.BytesDone,
		&t.CreatedAt, &t.StartedAt, &t.FinishedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Overwrite = overwrite != 0
	return &t, nil
}

// CreateTransfer records a new file push. The caller has already stored the
// payload at t.ObjectKey. Status is queued (agent offline) or running.
func (s *Store) CreateTransfer(t Transfer) error {
	started := int64(0)
	if t.Status == proto.RunRunning {
		started = now()
	}
	overwrite := 0
	if t.Overwrite {
		overwrite = 1
	}
	_, err := s.db.Exec(`INSERT INTO transfers
		(id, agent_id, filename, dest_path, size, hash, object_key, mode, overwrite, status, created_at, started_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.AgentID, t.Filename, t.DestPath, t.Size, t.Hash, t.ObjectKey,
		t.Mode, overwrite, t.Status, now(), started)
	return err
}

func (s *Store) GetTransfer(id string) (*Transfer, error) {
	return scanTransfer(s.db.QueryRow(`SELECT `+transferCols+` FROM transfers WHERE id = ?`, id).Scan)
}

func (s *Store) ListTransfers(agentID string, limit int) ([]Transfer, error) {
	q := `SELECT ` + transferCols + ` FROM transfers`
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
	var out []Transfer
	for rows.Next() {
		t, err := scanTransfer(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// QueuedTransfers returns pushes waiting for an agent to come online.
func (s *Store) QueuedTransfers(agentID string) ([]Transfer, error) {
	rows, err := s.db.Query(`SELECT `+transferCols+` FROM transfers
		WHERE agent_id = ? AND status = ? ORDER BY created_at, rowid`, agentID, proto.RunQueued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transfer
	for rows.Next() {
		t, err := scanTransfer(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *Store) MarkTransferStarted(id string) error {
	_, err := s.db.Exec(`UPDATE transfers SET status = ?, started_at = ? WHERE id = ?`,
		proto.RunRunning, now(), id)
	return err
}

func (s *Store) UpdateTransferProgress(id string, bytesDone int64) error {
	_, err := s.db.Exec(`UPDATE transfers SET bytes_done = ? WHERE id = ?`, bytesDone, id)
	return err
}

func (s *Store) FinishTransfer(id, status, writtenPath, errMsg string) error {
	_, err := s.db.Exec(`UPDATE transfers SET status = ?, written_path = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, writtenPath, errMsg, now(), id)
	return err
}

// FailRunningTransfers marks an agent's in-flight pushes failed when it
// disconnects mid-transfer; queued ones are left to dispatch on reconnect.
func (s *Store) FailRunningTransfers(agentID, reason string) error {
	_, err := s.db.Exec(`UPDATE transfers SET status = ?, error = ?, finished_at = ? WHERE agent_id = ? AND status = ?`,
		proto.RunError, reason, now(), agentID, proto.RunRunning)
	return err
}

// DeleteTransfer removes the record and returns its payload's storage key so
// the caller can delete the object from the backend.
func (s *Store) DeleteTransfer(id string) (objectKey string, err error) {
	t, err := s.GetTransfer(id)
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`DELETE FROM transfers WHERE id = ?`, id); err != nil {
		return "", err
	}
	return t.ObjectKey, nil
}

// TransferKeysForAgent lists the payload storage keys of every transfer of
// an agent, used to clean up the backend when the agent is deleted.
func (s *Store) TransferKeysForAgent(agentID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT object_key FROM transfers WHERE agent_id = ? AND object_key <> ''`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
