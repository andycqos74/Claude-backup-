package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"centralbackup/internal/proto"
)

// Jobs can be configured on the client by editing agent.yaml (or via the
// `backup-agent job` CLI, which edits the same file). The daemon watches
// the file and syncs changes to the server; the server pushes back its
// authoritative job set, which is written into the file. Conflicts resolve
// last-write-wins via the per-job version counter.
//
// Removing a job from the file does NOT delete it (it reappears on the next
// server push) — deletion is explicit, via the `delete:` list in the file
// (written by `backup-agent job rm`) or the server GUI.

// ConfigFile is the on-disk agent.yaml layout.
type ConfigFile struct {
	Jobs   []YamlJob `yaml:"jobs"`
	Delete []string  `yaml:"delete,omitempty"`
}

// YamlJob mirrors proto.Job for the file, with pointer booleans so that an
// omitted `enabled:` defaults to true instead of silently disabling a job.
type YamlJob struct {
	ID        string   `yaml:"id,omitempty"`
	Name      string   `yaml:"name"`
	Paths     []string `yaml:"paths"`
	Excludes  []string `yaml:"excludes,omitempty"`
	Schedule  string   `yaml:"schedule,omitempty"`
	FullEvery int      `yaml:"full_every,omitempty"`
	KeepLast  int      `yaml:"keep_last,omitempty"`
	KeepDays  int      `yaml:"keep_days,omitempty"`
	PreHook   string   `yaml:"pre_hook,omitempty"`
	PostHook  string   `yaml:"post_hook,omitempty"`
	Enabled   *bool    `yaml:"enabled,omitempty"`
	Catchup   *bool    `yaml:"catchup,omitempty"`
}

func (y YamlJob) toProto() proto.Job {
	enabled, catchup := true, true
	if y.Enabled != nil {
		enabled = *y.Enabled
	}
	if y.Catchup != nil {
		catchup = *y.Catchup
	}
	return proto.Job{
		ID: y.ID, Name: y.Name, Paths: y.Paths, Excludes: y.Excludes,
		Schedule: y.Schedule, FullEvery: y.FullEvery,
		KeepLast: y.KeepLast, KeepDays: y.KeepDays,
		PreHook: y.PreHook, PostHook: y.PostHook,
		Enabled: enabled, Catchup: catchup,
	}
}

func yamlFromProto(j proto.Job) YamlJob {
	e, c := j.Enabled, j.Catchup
	return YamlJob{
		ID: j.ID, Name: j.Name, Paths: j.Paths, Excludes: j.Excludes,
		Schedule: j.Schedule, FullEvery: j.FullEvery,
		KeepLast: j.KeepLast, KeepDays: j.KeepDays,
		PreHook: j.PreHook, PostHook: j.PostHook,
		Enabled: &e, Catchup: &c,
	}
}

func ReadConfigFile(path string) (*ConfigFile, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &ConfigFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cf ConfigFile
	if err := yaml.Unmarshal(b, &cf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cf, nil
}

func WriteConfigFile(path string, cf *ConfigFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	header := "# Backup jobs for this client. Edit freely - the agent syncs changes\n" +
		"# to the central server, and server-side edits are written back here.\n" +
		"# To delete a job use `backup-agent job rm <name>` (or the server GUI);\n" +
		"# simply removing it from this file will NOT delete it.\n"
	b, err := yaml.Marshal(cf)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), b...), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// localConfig is the daemon-side sync state machine for agent.yaml.
type localConfig struct {
	a           *Agent
	mu          sync.Mutex
	lastHash    string               // content hash last read/written by us
	serverJobs  map[string]proto.Job // authoritative set, by job id
	sentCreates map[string]bool      // job names with an in-flight create
}

func newLocalConfig(a *Agent) *localConfig {
	lc := &localConfig{a: a, serverJobs: map[string]proto.Job{}, sentCreates: map[string]bool{}}
	lc.loadCache()
	return lc
}

func (lc *localConfig) cachePath() string { return filepath.Join(lc.a.stateDir, "jobs-cache.json") }

func (lc *localConfig) loadCache() {
	b, err := os.ReadFile(lc.cachePath())
	if err != nil {
		return
	}
	json.Unmarshal(b, &lc.serverJobs)
}

func (lc *localConfig) saveCache() {
	b, _ := json.Marshal(lc.serverJobs)
	os.WriteFile(lc.cachePath(), b, 0o600)
}

func fileHash(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// watch polls agent.yaml for edits (simple and dependency-free; the file is
// tiny) and syncs when it changes.
func (lc *localConfig) watch(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			lc.mu.Lock()
			changed := fileHash(lc.a.configPath) != lc.lastHash
			lc.mu.Unlock()
			if changed {
				lc.syncNow()
			}
		}
	}
}

// syncNow diffs agent.yaml against the last known server state and sends
// creations, edits and deletions upstream. Called on file change and on
// every (re)connect.
func (lc *localConfig) syncNow() {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	cf, err := ReadConfigFile(lc.a.configPath)
	if err != nil {
		log.Printf("config: %v", err)
		return
	}
	lc.lastHash = fileHash(lc.a.configPath)

	var upd []proto.Job
	for _, yj := range cf.Jobs {
		j := yj.toProto()
		if j.Name == "" || len(j.Paths) == 0 {
			continue
		}
		if j.ID == "" {
			if lc.sentCreates[j.Name] {
				continue
			}
			lc.sentCreates[j.Name] = true
			upd = append(upd, j)
			continue
		}
		cur, ok := lc.serverJobs[j.ID]
		if !ok {
			continue // stale id: server will push the truth back
		}
		if !jobSpecEqual(j, cur) {
			j.Version = cur.Version + 1
			upd = append(upd, j)
		}
	}
	if len(upd) > 0 {
		if err := lc.a.send(proto.MsgJobsSync, proto.JobsSync{Jobs: upd}); err != nil {
			// Offline: forget in-flight creates so they retry on reconnect.
			for _, j := range upd {
				if j.ID == "" {
					delete(lc.sentCreates, j.Name)
				}
			}
		} else {
			log.Printf("config: synced %d job change(s) to server", len(upd))
		}
	}
	for _, id := range cf.Delete {
		if err := lc.a.send(proto.MsgJobDelete, proto.JobDelete{JobID: id}); err == nil {
			log.Printf("config: requested deletion of job %s", id)
		}
	}
}

// applyServerJobs handles the authoritative job set pushed by the server:
// update the cache and rewrite agent.yaml to match.
func (lc *localConfig) applyServerJobs(jobs []proto.Job) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	lc.serverJobs = map[string]proto.Job{}
	lc.sentCreates = map[string]bool{}
	cf := &ConfigFile{}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	for _, j := range jobs {
		lc.serverJobs[j.ID] = j
		cf.Jobs = append(cf.Jobs, yamlFromProto(j))
	}
	lc.saveCache()

	if err := WriteConfigFile(lc.a.configPath, cf); err != nil {
		log.Printf("config: write %s: %v", lc.a.configPath, err)
		return
	}
	lc.lastHash = fileHash(lc.a.configPath)
	log.Printf("config: %d job(s) synced from server", len(jobs))

	// Local jobs the agent can run need to be visible to hooks/scheduler on
	// the server only; nothing further to do here.
}

// jobSpecEqual compares the user-editable spec fields (ignores identity and
// versioning metadata).
func jobSpecEqual(a, b proto.Job) bool {
	a.AgentID, a.Origin, a.Version = "", "", 0
	b.AgentID, b.Origin, b.Version = "", "", 0
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
