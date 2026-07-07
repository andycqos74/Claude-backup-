// Package store persists all server metadata in a single SQLite database:
// admin users and sessions, enrolled agents, jobs, runs, snapshots and the
// blob index. Backup payload data itself lives in the storage backend, not
// here.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"

	"centralbackup/internal/proto"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	// modernc sqlite: single writer; busy_timeout smooths concurrent access.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	username TEXT UNIQUE NOT NULL,
	password_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS enroll_tokens (
	token TEXT PRIMARY KEY,
	note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_by TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS agents (
	id TEXT PRIMARY KEY,
	secret_hash TEXT NOT NULL,
	name TEXT NOT NULL,
	hostname TEXT NOT NULL DEFAULT '',
	os TEXT NOT NULL DEFAULT '',
	arch TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	last_seen INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS jobs (
	id TEXT PRIMARY KEY,
	agent_id TEXT NOT NULL,
	origin TEXT NOT NULL,
	version INTEGER NOT NULL,
	run_count INTEGER NOT NULL DEFAULT 0,
	catchup_pending INTEGER NOT NULL DEFAULT 0,
	spec TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
	id TEXT PRIMARY KEY,
	job_id TEXT NOT NULL DEFAULT '',
	agent_id TEXT NOT NULL,
	mode TEXT NOT NULL,
	status TEXT NOT NULL,
	snapshot_id TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	stats TEXT NOT NULL DEFAULT '',
	progress TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	started_at INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_runs_agent ON runs(agent_id, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_job ON runs(job_id, created_at);
CREATE TABLE IF NOT EXISTS run_logs (
	run_id TEXT NOT NULL,
	ts INTEGER NOT NULL,
	level TEXT NOT NULL,
	message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_logs ON run_logs(run_id, ts);
CREATE TABLE IF NOT EXISTS snapshots (
	id TEXT PRIMARY KEY,
	job_id TEXT NOT NULL,
	agent_id TEXT NOT NULL,
	run_id TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL,
	files INTEGER NOT NULL DEFAULT 0,
	bytes INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	manifest_key TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_job ON snapshots(job_id, created_at);
CREATE TABLE IF NOT EXISTS blobs (
	hash TEXT PRIMARY KEY,
	size_raw INTEGER NOT NULL,
	size_stored INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
`)
	return err
}

func NewID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func NewSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// hashSecret hashes high-entropy random secrets (agent keys, session and
// enrollment tokens). SHA-256 is appropriate here because the inputs are
// 256-bit random values, not guessable passwords.
func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func now() int64 { return time.Now().Unix() }

// ---- users & sessions ----

type User struct {
	ID       string
	Username string
}

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES (?,?,?,?)`,
		NewID(), username, string(hash), now())
	return err
}

func (s *Store) Authenticate(username, password string) (*User, error) {
	var u User
	var hash string
	err := s.db.QueryRow(`SELECT id, username, password_hash FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &hash)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, ErrNotFound
	}
	return &u, nil
}

func (s *Store) SetPassword(userID, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), userID)
	return err
}

func (s *Store) CreateSession(userID string, ttl time.Duration) (string, error) {
	token := NewSecret()
	_, err := s.db.Exec(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?,?,?)`,
		hashSecret(token), userID, time.Now().Add(ttl).Unix())
	return token, err
}

func (s *Store) SessionUser(token string) (*User, error) {
	var u User
	err := s.db.QueryRow(`
		SELECT u.id, u.username FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token = ? AND s.expires_at > ?`, hashSecret(token), now()).
		Scan(&u.ID, &u.Username)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return &u, err
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, hashSecret(token))
	return err
}

// ---- enrollment tokens ----

type EnrollToken struct {
	Token     string // only set on creation; stored hashed
	Note      string
	CreatedAt int64
	ExpiresAt int64
	UsedBy    string
}

func (s *Store) CreateEnrollToken(note string, ttl time.Duration) (string, error) {
	token := NewSecret()
	_, err := s.db.Exec(`INSERT INTO enroll_tokens (token, note, created_at, expires_at) VALUES (?,?,?,?)`,
		hashSecret(token), note, now(), time.Now().Add(ttl).Unix())
	return token, err
}

func (s *Store) ListEnrollTokens() ([]EnrollToken, error) {
	rows, err := s.db.Query(`SELECT note, created_at, expires_at, used_by FROM enroll_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnrollToken
	for rows.Next() {
		var t EnrollToken
		if err := rows.Scan(&t.Note, &t.CreatedAt, &t.ExpiresAt, &t.UsedBy); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ConsumeEnrollToken marks a valid unused token as used and returns nil.
func (s *Store) ConsumeEnrollToken(token, agentID string) error {
	res, err := s.db.Exec(`UPDATE enroll_tokens SET used_by = ? WHERE token = ? AND used_by = '' AND expires_at > ?`,
		agentID, hashSecret(token), now())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- agents ----

type Agent struct {
	ID       string
	Name     string
	Hostname string
	OS       string
	Arch     string
	Version  string
	Created  int64
	LastSeen int64
}

func (s *Store) CreateAgent(name string, req proto.EnrollRequest) (id, secret string, err error) {
	id, secret = NewID(), NewSecret()
	if name == "" {
		name = req.Hostname
	}
	if name == "" {
		name = "agent-" + id[:6]
	}
	_, err = s.db.Exec(`INSERT INTO agents (id, secret_hash, name, hostname, os, arch, created_at) VALUES (?,?,?,?,?,?,?)`,
		id, hashSecret(secret), name, req.Hostname, req.OS, req.Arch, now())
	return id, secret, err
}

func (s *Store) VerifyAgent(id, secret string) bool {
	var stored string
	if err := s.db.QueryRow(`SELECT secret_hash FROM agents WHERE id = ?`, id).Scan(&stored); err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(hashSecret(secret))) == 1
}

func (s *Store) GetAgent(id string) (*Agent, error) {
	var a Agent
	err := s.db.QueryRow(`SELECT id, name, hostname, os, arch, version, created_at, last_seen FROM agents WHERE id = ?`, id).
		Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Arch, &a.Version, &a.Created, &a.LastSeen)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return &a, err
}

func (s *Store) ListAgents() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT id, name, hostname, os, arch, version, created_at, last_seen FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.Name, &a.Hostname, &a.OS, &a.Arch, &a.Version, &a.Created, &a.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) TouchAgent(id string, hello *proto.Hello) error {
	if hello != nil {
		_, err := s.db.Exec(`UPDATE agents SET last_seen = ?, hostname = ?, os = ?, arch = ?, version = ? WHERE id = ?`,
			now(), hello.Hostname, hello.OS, hello.Arch, hello.Version, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE agents SET last_seen = ? WHERE id = ?`, now(), id)
	return err
}

func (s *Store) RenameAgent(id, name string) error {
	_, err := s.db.Exec(`UPDATE agents SET name = ? WHERE id = ?`, name, id)
	return err
}

// DeleteAgent removes the agent and its jobs/runs. Snapshots are removed by
// the caller first (they reference storage objects).
func (s *Store) DeleteAgent(id string) error {
	for _, q := range []string{
		`DELETE FROM run_logs WHERE run_id IN (SELECT id FROM runs WHERE agent_id = ?)`,
		`DELETE FROM runs WHERE agent_id = ?`,
		`DELETE FROM jobs WHERE agent_id = ?`,
		`DELETE FROM agents WHERE id = ?`,
	} {
		if _, err := s.db.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// ---- jobs ----

type JobRow struct {
	Job            proto.Job
	RunCount       int64
	CatchupPending bool
	UpdatedAt      int64
}

func (s *Store) scanJob(scan func(dest ...any) error) (*JobRow, error) {
	var spec string
	var r JobRow
	var catchup int
	if err := scan(&spec, &r.RunCount, &catchup, &r.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(spec), &r.Job); err != nil {
		return nil, fmt.Errorf("corrupt job spec: %w", err)
	}
	r.CatchupPending = catchup != 0
	return &r, nil
}

func (s *Store) GetJob(id string) (*JobRow, error) {
	row := s.db.QueryRow(`SELECT spec, run_count, catchup_pending, updated_at FROM jobs WHERE id = ?`, id)
	return s.scanJob(row.Scan)
}

func (s *Store) ListJobs(agentID string) ([]JobRow, error) {
	q := `SELECT spec, run_count, catchup_pending, updated_at FROM jobs`
	var args []any
	if agentID != "" {
		q += ` WHERE agent_id = ?`
		args = append(args, agentID)
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		r, err := s.scanJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// SaveJob inserts or updates a job spec verbatim (caller manages Version).
func (s *Store) SaveJob(j proto.Job) error {
	spec, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO jobs (id, agent_id, origin, version, spec, updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET agent_id=excluded.agent_id, origin=excluded.origin,
			version=excluded.version, spec=excluded.spec, updated_at=excluded.updated_at`,
		j.ID, j.AgentID, j.Origin, j.Version, string(spec), now())
	return err
}

func (s *Store) DeleteJob(id string) error {
	_, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id)
	return err
}

func (s *Store) IncrementRunCount(id string) error {
	_, err := s.db.Exec(`UPDATE jobs SET run_count = run_count + 1 WHERE id = ?`, id)
	return err
}

func (s *Store) SetCatchupPending(id string, pending bool) error {
	v := 0
	if pending {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE jobs SET catchup_pending = ? WHERE id = ?`, v, id)
	return err
}

// ---- runs ----

type Run struct {
	ID         string
	JobID      string
	AgentID    string
	Mode       string
	Status     string
	SnapshotID string
	Error      string
	Stats      string // JSON proto.RunStats
	Progress   string // JSON proto.RunProgress
	CreatedAt  int64
	StartedAt  int64
	FinishedAt int64
}

func (s *Store) CreateRun(jobID, agentID, mode, status string) (string, error) {
	id := NewID()
	started := int64(0)
	if status == proto.RunRunning {
		started = now()
	}
	_, err := s.db.Exec(`INSERT INTO runs (id, job_id, agent_id, mode, status, created_at, started_at) VALUES (?,?,?,?,?,?,?)`,
		id, jobID, agentID, mode, status, now(), started)
	return id, err
}

func (s *Store) GetRun(id string) (*Run, error) {
	var r Run
	err := s.db.QueryRow(`SELECT id, job_id, agent_id, mode, status, snapshot_id, error, stats, progress, created_at, started_at, finished_at
		FROM runs WHERE id = ?`, id).
		Scan(&r.ID, &r.JobID, &r.AgentID, &r.Mode, &r.Status, &r.SnapshotID, &r.Error, &r.Stats, &r.Progress, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return &r, err
}

func (s *Store) ListRuns(agentID, jobID string, limit int) ([]Run, error) {
	q := `SELECT id, job_id, agent_id, mode, status, snapshot_id, error, stats, progress, created_at, started_at, finished_at FROM runs`
	var conds []string
	var args []any
	if agentID != "" {
		conds = append(conds, `agent_id = ?`)
		args = append(args, agentID)
	}
	if jobID != "" {
		conds = append(conds, `job_id = ?`)
		args = append(args, jobID)
	}
	for i, c := range conds {
		if i == 0 {
			q += ` WHERE ` + c
		} else {
			q += ` AND ` + c
		}
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.JobID, &r.AgentID, &r.Mode, &r.Status, &r.SnapshotID, &r.Error, &r.Stats, &r.Progress, &r.CreatedAt, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueuedRuns returns runs waiting for an agent to come online.
func (s *Store) QueuedRuns(agentID string) ([]Run, error) {
	rows, err := s.db.Query(`SELECT id, job_id, agent_id, mode, status, snapshot_id, error, stats, progress, created_at, started_at, finished_at
		FROM runs WHERE agent_id = ? AND status = ? ORDER BY created_at`, agentID, proto.RunQueued)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.JobID, &r.AgentID, &r.Mode, &r.Status, &r.SnapshotID, &r.Error, &r.Stats, &r.Progress, &r.CreatedAt, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) MarkRunStarted(id string) error {
	_, err := s.db.Exec(`UPDATE runs SET status = ?, started_at = ? WHERE id = ?`, proto.RunRunning, now(), id)
	return err
}

func (s *Store) UpdateRunProgress(id string, progress proto.RunProgress) error {
	b, _ := json.Marshal(progress)
	_, err := s.db.Exec(`UPDATE runs SET progress = ? WHERE id = ?`, string(b), id)
	return err
}

func (s *Store) FinishRun(id, status, snapshotID, errMsg string, stats proto.RunStats) error {
	b, _ := json.Marshal(stats)
	_, err := s.db.Exec(`UPDATE runs SET status = ?, snapshot_id = ?, error = ?, stats = ?, finished_at = ? WHERE id = ?`,
		status, snapshotID, errMsg, string(b), now(), id)
	return err
}

// FailRunningRuns marks all running/queued runs of an agent as failed (used
// when the agent disconnects mid-run; queued runs are kept).
func (s *Store) FailRunningRuns(agentID, reason string) error {
	_, err := s.db.Exec(`UPDATE runs SET status = ?, error = ?, finished_at = ? WHERE agent_id = ? AND status = ?`,
		proto.RunError, reason, now(), agentID, proto.RunRunning)
	return err
}

func (s *Store) AppendRunLog(runID, level, message string) error {
	_, err := s.db.Exec(`INSERT INTO run_logs (run_id, ts, level, message) VALUES (?,?,?,?)`,
		runID, time.Now().UnixMilli(), level, message)
	return err
}

type RunLogLine struct {
	TS      int64
	Level   string
	Message string
}

func (s *Store) RunLogs(runID string) ([]RunLogLine, error) {
	rows, err := s.db.Query(`SELECT ts, level, message FROM run_logs WHERE run_id = ? ORDER BY ts, rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunLogLine
	for rows.Next() {
		var l RunLogLine
		if err := rows.Scan(&l.TS, &l.Level, &l.Message); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---- snapshots ----

type Snapshot struct {
	ID          string
	JobID       string
	AgentID     string
	RunID       string
	Mode        string
	Files       int64
	Bytes       int64
	CreatedAt   int64
	ManifestKey string
}

func (s *Store) CreateSnapshot(sn Snapshot) error {
	_, err := s.db.Exec(`INSERT INTO snapshots (id, job_id, agent_id, run_id, mode, files, bytes, created_at, manifest_key)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		sn.ID, sn.JobID, sn.AgentID, sn.RunID, sn.Mode, sn.Files, sn.Bytes, now(), sn.ManifestKey)
	return err
}

func (s *Store) GetSnapshot(id string) (*Snapshot, error) {
	var sn Snapshot
	err := s.db.QueryRow(`SELECT id, job_id, agent_id, run_id, mode, files, bytes, created_at, manifest_key FROM snapshots WHERE id = ?`, id).
		Scan(&sn.ID, &sn.JobID, &sn.AgentID, &sn.RunID, &sn.Mode, &sn.Files, &sn.Bytes, &sn.CreatedAt, &sn.ManifestKey)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return &sn, err
}

func (s *Store) ListSnapshots(agentID, jobID string) ([]Snapshot, error) {
	q := `SELECT id, job_id, agent_id, run_id, mode, files, bytes, created_at, manifest_key FROM snapshots`
	var conds []string
	var args []any
	if agentID != "" {
		conds = append(conds, `agent_id = ?`)
		args = append(args, agentID)
	}
	if jobID != "" {
		conds = append(conds, `job_id = ?`)
		args = append(args, jobID)
	}
	for i, c := range conds {
		if i == 0 {
			q += ` WHERE ` + c
		} else {
			q += ` AND ` + c
		}
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var sn Snapshot
		if err := rows.Scan(&sn.ID, &sn.JobID, &sn.AgentID, &sn.RunID, &sn.Mode, &sn.Files, &sn.Bytes, &sn.CreatedAt, &sn.ManifestKey); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// LatestSnapshot returns the most recent snapshot for a job, or ErrNotFound.
func (s *Store) LatestSnapshot(jobID string) (*Snapshot, error) {
	var sn Snapshot
	err := s.db.QueryRow(`SELECT id, job_id, agent_id, run_id, mode, files, bytes, created_at, manifest_key
		FROM snapshots WHERE job_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, jobID).
		Scan(&sn.ID, &sn.JobID, &sn.AgentID, &sn.RunID, &sn.Mode, &sn.Files, &sn.Bytes, &sn.CreatedAt, &sn.ManifestKey)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return &sn, err
}

func (s *Store) DeleteSnapshot(id string) error {
	_, err := s.db.Exec(`DELETE FROM snapshots WHERE id = ?`, id)
	return err
}

// ---- blobs ----

func (s *Store) HasBlob(hash string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM blobs WHERE hash = ?`, hash).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) MissingBlobs(hashes []string) ([]string, error) {
	var missing []string
	for _, h := range hashes {
		ok, err := s.HasBlob(h)
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, h)
		}
	}
	return missing, nil
}

func (s *Store) AddBlob(hash string, sizeRaw, sizeStored int64) error {
	_, err := s.db.Exec(`INSERT INTO blobs (hash, size_raw, size_stored, created_at) VALUES (?,?,?,?)
		ON CONFLICT(hash) DO NOTHING`, hash, sizeRaw, sizeStored, now())
	return err
}

func (s *Store) DeleteBlob(hash string) error {
	_, err := s.db.Exec(`DELETE FROM blobs WHERE hash = ?`, hash)
	return err
}

// AllBlobs returns hash -> created_at for GC mark/sweep.
func (s *Store) AllBlobs() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT hash, created_at FROM blobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var h string
		var t int64
		if err := rows.Scan(&h, &t); err != nil {
			return nil, err
		}
		out[h] = t
	}
	return out, rows.Err()
}

type StorageStats struct {
	Blobs      int64
	SizeRaw    int64
	SizeStored int64
	Snapshots  int64
}

func (s *Store) Stats() (StorageStats, error) {
	var st StorageStats
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size_raw),0), COALESCE(SUM(size_stored),0) FROM blobs`).
		Scan(&st.Blobs, &st.SizeRaw, &st.SizeStored); err != nil {
		return st, err
	}
	err := s.db.QueryRow(`SELECT COUNT(*) FROM snapshots`).Scan(&st.Snapshots)
	return st, err
}
