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
	MsgHello           = "hello"
	MsgJobsSync        = "jobs_sync"
	MsgJobDelete       = "job_delete"
	MsgRunProgress     = "run_progress"
	MsgRunLog          = "run_log"
	MsgRunDone         = "run_done"
	MsgDockerInventory = "docker_inventory"

	// agent -> server (file push)
	MsgTransferProgress = "transfer_progress"
	MsgTransferDone     = "transfer_done"

	// agent -> server (file browser)
	MsgDirListing = "dir_listing"

	// agent -> server (remote command console)
	MsgCommandOutput = "command_output"
	MsgCommandDone   = "command_done"

	// server -> agent
	MsgJobsUpdate     = "jobs_update"
	MsgRunBackup      = "run_backup"
	MsgRestore        = "restore"
	MsgCancelRun      = "cancel_run"
	MsgDiscoverDocker = "discover_docker"

	// server -> agent (file push/pull)
	MsgPushFile       = "push_file"
	MsgPullFile       = "pull_file"
	MsgCancelTransfer = "cancel_transfer"

	// server -> agent (file browser)
	MsgBrowseDir = "browse_dir"

	// server -> agent (remote command console)
	MsgRunCommand    = "run_command"
	MsgCancelCommand = "cancel_command"
)

// Transfer directions.
const (
	DirectionPush = "push" // server -> client
	DirectionPull = "pull" // client -> server
)

// Command shells. powershell and cmd are Windows-only; sh is used on
// Linux/macOS.
const (
	ShellPowerShell = "powershell"
	ShellCmd        = "cmd"
	ShellSh         = "sh"
)

// Run modes.
const (
	ModeFull        = "full"
	ModeIncremental = "incremental"
	ModeRestore     = "restore"
)

// Run statuses.
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunSuccess   = "success"
	RunPartial   = "partial" // finished but some files were skipped
	RunError     = "error"
	RunCancelled = "cancelled"
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
	Mode           string `json:"mode"`                       // full | incremental
	PrevSnapshotID string `json:"prev_snapshot_id,omitempty"` // for incremental diff if local cache is missing
}

// Restore instructs the agent to restore files from a snapshot.
type Restore struct {
	RunID      string   `json:"run_id"`
	SnapshotID string   `json:"snapshot_id"`
	Paths      []string `json:"paths,omitempty"`      // path prefixes to restore; empty = everything
	TargetDir  string   `json:"target_dir,omitempty"` // empty = original locations
	Overwrite  bool     `json:"overwrite"`
}

// CancelRun asks the agent to stop an in-progress run (backup or restore).
// The agent finishes cleanly with RunDone{Status: RunCancelled} rather than
// dropping the connection or leaving a half-committed snapshot.
type CancelRun struct {
	RunID string `json:"run_id"`
}

// DiscoverDocker asks the agent to enumerate the Docker containers on its
// host, so the GUI can offer them as tick-boxes instead of hand-typed paths.
type DiscoverDocker struct {
	RequestID string `json:"request_id"`
}

// DockerMount is one volume or bind mount of a container. Source is the
// path on the *host*; BackupPath is the same location as the agent would
// have to address it in a job (prefixed with the host-root mount when the
// agent is itself containerised), and is what a job should actually use.
type DockerMount struct {
	Type        string `json:"type"` // volume | bind
	Name        string `json:"name,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	BackupPath  string `json:"backup_path"`
}

// Container kinds. The GUI treats them differently because a live database
// must be dumped, not file-copied.
const (
	DockerKindDatabase  = "database" // needs a dump hook
	DockerKindEmbedded  = "embedded" // SQLite/BoltDB; copyable, better with stop/start
	DockerKindFiles     = "files"    // plain files, copy directly
	DockerKindStateless = "stateless"
)

// DockerContainer is one container as offered in the job editor.
type DockerContainer struct {
	Name    string        `json:"name"`
	Image   string        `json:"image"`
	Stack   string        `json:"stack,omitempty"`   // compose project, if any
	Service string        `json:"service,omitempty"` // compose service, if any
	State   string        `json:"state"`
	Kind    string        `json:"kind"`
	Engine  string        `json:"engine,omitempty"` // mysql | postgres | mssql | mongo | redis
	Mounts  []DockerMount `json:"mounts,omitempty"`
	// Note is a human-readable caveat shown next to the container, e.g.
	// that its data directory does not look persisted.
	Note string `json:"note,omitempty"`
}

// DockerInventory is the agent's answer to DiscoverDocker.
type DockerInventory struct {
	RequestID  string            `json:"request_id"`
	Available  bool              `json:"available"` // false = no Docker socket reachable
	Error      string            `json:"error,omitempty"`
	HostRoot   string            `json:"host_root,omitempty"` // e.g. "/host" when containerised
	Containers []DockerContainer `json:"containers,omitempty"`
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
	Status     string   `json:"status"` // success | partial | error | cancelled
	SnapshotID string   `json:"snapshot_id,omitempty"`
	Stats      RunStats `json:"stats"`
	Error      string   `json:"error,omitempty"`
}

// PushFile instructs the agent to fetch one file the admin uploaded on the
// server and write it to the client's disk in the background. The agent is
// a headless service, so nothing appears on the client's screen; progress
// is reported back to the server only. The payload itself travels over the
// authenticated HTTPS data plane (like blobs), not the control socket.
type PushFile struct {
	TransferID string `json:"transfer_id"`
	// DestPath is where to write on the client. A path ending in a slash or
	// backslash (or naming an existing directory) is treated as a target
	// directory and Filename is appended; otherwise it is the full target
	// filename.
	DestPath  string `json:"dest_path"`
	Filename  string `json:"filename"`
	Hash      string `json:"hash"` // sha256 hex of the raw content, verified on arrival
	Size      int64  `json:"size"`
	Mode      uint32 `json:"mode,omitempty"` // unix perms; ignored on Windows
	Overwrite bool   `json:"overwrite"`
}

// PullFile instructs the agent to read one file off the client's disk and
// upload it to the server, where the operator can then download it. Like a
// push, it runs in the agent's background service and is observed only from
// the server. The payload travels over the authenticated HTTPS data plane.
type PullFile struct {
	TransferID string `json:"transfer_id"`
	SourcePath string `json:"source_path"` // absolute path of the file to fetch off the client
}

// BrowseDir asks the agent to list one directory on the client so the server
// GUI can show a file browser. An empty Path means "the filesystem roots"
// (drive letters on Windows, "/" elsewhere).
type BrowseDir struct {
	RequestID string `json:"request_id"`
	Path      string `json:"path"`
}

// DirEntry is one item in a browsed directory.
type DirEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // full path, so the GUI can navigate without re-joining
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
	Mtime int64  `json:"mtime,omitempty"` // unix seconds
}

// DirListing is the agent's answer to BrowseDir.
type DirListing struct {
	RequestID string     `json:"request_id"`
	Path      string     `json:"path"`   // the (cleaned, absolute) path listed
	Parent    string     `json:"parent"` // parent path for an "up" control ("" at a root)
	Entries   []DirEntry `json:"entries,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// TransferProgress is streamed while a pushed file is being written.
type TransferProgress struct {
	TransferID string `json:"transfer_id"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
}

// TransferDone finalises a file push. Status is one of the Run* terminal
// statuses (success | error | cancelled).
type TransferDone struct {
	TransferID string `json:"transfer_id"`
	Status     string `json:"status"`
	Path       string `json:"path,omitempty"` // the absolute path actually written
	Error      string `json:"error,omitempty"`
}

// CancelTransfer asks the agent to abort an in-progress file push.
type CancelTransfer struct {
	TransferID string `json:"transfer_id"`
}

// RunCommand asks the agent to run a shell command on the client and stream
// its output back. It runs in the agent's background service (as that
// service's account — LocalSystem or root), so nothing appears on the
// client's screen; the operator sees the output only in the server GUI.
type RunCommand struct {
	CommandID string `json:"command_id"`
	Shell     string `json:"shell"` // powershell | cmd | sh
	Command   string `json:"command"`
}

// CancelCommand asks the agent to terminate a running command.
type CancelCommand struct {
	CommandID string `json:"command_id"`
}

// CommandCancelled is the exact CommandDone.Error the agent sends when a
// command was terminated by a cancel request, so the server can distinguish
// it from a genuine failure.
const CommandCancelled = "cancelled"

// CommandOutput is one line of a running command's output.
type CommandOutput struct {
	CommandID string `json:"command_id"`
	Stream    string `json:"stream"` // stdout | stderr
	Line      string `json:"line"`
}

// CommandDone finalises a command run. ExitCode is the process exit status
// (0 = success); Error is set only when the command could not be launched or
// was cancelled.
type CommandDone struct {
	CommandID string `json:"command_id"`
	ExitCode  int    `json:"exit_code"`
	Error     string `json:"error,omitempty"`
}

// ManifestEntry is one line of a snapshot manifest (JSONL, zstd-compressed
// at rest). Paths are absolute, exactly as seen on the client.
type ManifestEntry struct {
	Type   string `json:"t"` // f | d | l
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
