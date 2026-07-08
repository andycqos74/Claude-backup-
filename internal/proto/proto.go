// Package proto defines the message types shared between the central server
// and remote agents. Agents connect outbound over WSS; every frame is an
// Envelope carrying one of the typed payloads below. Bulk data (blobs,
// manifests) travels over authenticated HTTPS endpoints, not the socket.
package proto

import "encoding/json"

// Envelope wraps every WebSocket frame in both directions.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Wrap marshals a payload into an Envelope.
func Wrap(msgType string, payload any) (Envelope, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Type: msgType, Data: b}, nil
}

// Message type constants.
const (
	// agent -> server
	MsgHello       = "hello"
	MsgJobsSync    = "jobs_sync"
	MsgJobDelete   = "job_delete"
	MsgRunProgress = "run_progress"
	MsgRunLog      = "run_log"
	MsgRunDone     = "run_done"

	// server -> agent
	MsgJobsUpdate = "jobs_update"
	MsgRunBackup  = "run_backup"
	MsgRestore    = "restore"
)

// Run modes.
const (
	ModeFull        = "full"
	ModeIncremental = "incremental"
	ModeRestore     = "restore"
)

// Run statuses.
const (
	RunQueued  = "queued"
	RunRunning = "running"
	RunSuccess = "success"
	RunPartial = "partial" // finished but some files were skipped
	RunError   = "error"
)

// Job origins.
const (
	OriginServer = "server"
	OriginClient = "client"
)

// Job describes one backup job: what to back up on which agent, when, and
// how long to keep it. Jobs are editable from both the server GUI and the
// agent's local agent.yaml; Version implements last-write-wins sync.
type Job struct {
	ID        string   `json:"id" yaml:"id"`
	AgentID   string   `json:"agent_id" yaml:"-"`
	Name      string   `json:"name" yaml:"name"`
	Paths     []string `json:"paths" yaml:"paths"`
	Excludes  []string `json:"excludes,omitempty" yaml:"excludes,omitempty"`
	Schedule  string   `json:"schedule,omitempty" yaml:"schedule,omitempty"`     // cron; empty = manual only
	FullEvery int      `json:"full_every,omitempty" yaml:"full_every,omitempty"` // every Nth scheduled run is full; 0 = always incremental
	KeepLast  int      `json:"keep_last,omitempty" yaml:"keep_last,omitempty"`   // retention: keep newest N snapshots (0 = keep all)
	KeepDays  int      `json:"keep_days,omitempty" yaml:"keep_days,omitempty"`   // retention: also keep anything newer than N days
	PreHook   string   `json:"pre_hook,omitempty" yaml:"pre_hook,omitempty"`
	PostHook  string   `json:"post_hook,omitempty" yaml:"post_hook,omitempty"`
	Enabled   bool     `json:"enabled" yaml:"enabled"`
	Catchup   bool     `json:"catchup,omitempty" yaml:"catchup,omitempty"` // run missed schedule when agent reconnects
	Origin    string   `json:"origin" yaml:"-"`
	Version   int64    `json:"version" yaml:"-"`
}

// Hello is sent by the agent right after the socket is established.
type Hello struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
}

// JobsSync carries client-side job creations/edits up to the server.
// New jobs have an empty ID; the server assigns one and answers with a
// full JobsUpdate.
type JobsSync struct {
	Jobs []Job `json:"jobs"`
}

// JobDelete asks the server to delete a job (issued by `agent job rm`).
type JobDelete struct {
	JobID string `json:"job_id"`
}

// JobsUpdate is the server's authoritative full job set for this agent.
type JobsUpdate struct {
	Jobs []Job `json:"jobs"`
}

// RunBackup instructs the agent to execute a backup run.
type RunBackup struct {
	RunID          string `json:"run_id"`
	Job            Job    `json:"job"`
	Mode           string `json:"mode"`                      // full | incremental
	PrevSnapshotID string `json:"prev_snapshot_id,omitempty"` // for incremental diff if local cache is missing
}

// Restore instructs the agent to restore files from a snapshot.
type Restore struct {
	RunID      string   `json:"run_id"`
	SnapshotID string   `json:"snapshot_id"`
	Paths      []string `json:"paths,omitempty"`  // path prefixes to restore; empty = everything
	TargetDir  string   `json:"target_dir,omitempty"` // empty = original locations
	Overwrite  bool     `json:"overwrite"`
}

// RunProgress is streamed while a run is in flight.
type RunProgress struct {
	RunID      string `json:"run_id"`
	FilesDone  int64  `json:"files_done"`
	FilesTotal int64  `json:"files_total"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	Phase      string `json:"phase"` // scanning | uploading | restoring
}

// RunLog is a single log line from a run.
type RunLog struct {
	RunID   string `json:"run_id"`
	Level   string `json:"level"` // info | warn | error
	Message string `json:"message"`
}

// RunStats summarises a finished run.
type RunStats struct {
	FilesTotal    int64 `json:"files_total"`
	FilesChanged  int64 `json:"files_changed"`
	FilesSkipped  int64 `json:"files_skipped"`
	BytesTotal    int64 `json:"bytes_total"`
	BytesUploaded int64 `json:"bytes_uploaded"`
	DurationMS    int64 `json:"duration_ms"`
}

// RunDone finalises a run.
type RunDone struct {
	RunID      string   `json:"run_id"`
	Status     string   `json:"status"` // success | partial | error
	SnapshotID string   `json:"snapshot_id,omitempty"`
	Stats      RunStats `json:"stats"`
	Error      string   `json:"error,omitempty"`
}

// ManifestEntry is one line of a snapshot manifest (JSONL, zstd-compressed
// at rest). Paths are absolute, exactly as seen on the client.
type ManifestEntry struct {
	Type   string `json:"t"`            // f | d | l
	Path   string `json:"p"`
	Size   int64  `json:"s,omitempty"`
	Mode   uint32 `json:"m,omitempty"`
	Mtime  int64  `json:"mt,omitempty"` // unix nanoseconds
	Hash   string `json:"h,omitempty"`  // sha256 hex of file content
	Target string `json:"lt,omitempty"` // symlink target
}

// EnrollRequest is POSTed by a new agent with a one-time token.
type EnrollRequest struct {
	Token    string `json:"token"`
	Name     string `json:"name,omitempty"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// EnrollResponse hands the agent its permanent credentials.
type EnrollResponse struct {
	AgentID string `json:"agent_id"`
	Secret  string `json:"secret"`
}

// BlobCheckRequest / Response implement "which of these do you already have".
type BlobCheckRequest struct {
	Hashes []string `json:"hashes"`
}

type BlobCheckResponse struct {
	Missing []string `json:"missing"`
}

// SnapshotCommitResponse is returned when a manifest is committed.
type SnapshotCommitResponse struct {
	SnapshotID string `json:"snapshot_id"`
}
