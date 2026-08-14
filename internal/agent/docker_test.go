package agent

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"centralbackup/internal/proto"
)

func mount(typ, name, src, dst string) struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
} {
	return struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	}{typ, name, src, dst}
}

func TestBackupPathFor(t *testing.T) {
	cases := []struct{ hostPath, hostRoot, want string }{
		// Containerised agent: the host is under /host.
		{"/var/lib/docker/volumes/v/_data", "/host", "/host/var/lib/docker/volumes/v/_data"},
		{"/root/local-files", "/host", "/host/root/local-files"},
		// Native agent: paths are already correct.
		{"/var/lib/docker/volumes/v/_data", "", "/var/lib/docker/volumes/v/_data"},
		// Non-absolute sources (anonymous volumes) are left alone.
		{"", "/host", ""},
	}
	for _, c := range cases {
		if got := backupPathFor(c.hostPath, c.hostRoot); got != c.want {
			t.Errorf("backupPathFor(%q, %q) = %q, want %q", c.hostPath, c.hostRoot, got, c.want)
		}
	}
}

func TestDescribeContainerClassifiesEngines(t *testing.T) {
	cases := []struct {
		image      string
		wantKind   string
		wantEngine string
	}{
		{"mysql:8.4", proto.DockerKindDatabase, "mysql"},
		{"mysql:5.7", proto.DockerKindDatabase, "mysql"},
		{"mariadb:11", proto.DockerKindDatabase, "mysql"},
		{"postgres:16", proto.DockerKindDatabase, "postgres"},
		{"mcr.microsoft.com/mssql/server:2019-latest", proto.DockerKindDatabase, "mssql"},
		{"mongo:7", proto.DockerKindDatabase, "mongo"},
		{"redis:7-alpine", proto.DockerKindDatabase, "redis"},
		{"vaultwarden/server:latest", proto.DockerKindEmbedded, ""},
		{"docker.n8n.io/n8nio/n8n", proto.DockerKindEmbedded, ""},
		{"portainer/portainer-ce:latest", proto.DockerKindEmbedded, ""},
		{"b4bz/homer:latest", proto.DockerKindFiles, ""},
	}
	for _, c := range cases {
		api := dockerAPIContainer{
			Names: []string{"/x"},
			Image: c.image,
			Mounts: []struct {
				Type        string `json:"Type"`
				Name        string `json:"Name"`
				Source      string `json:"Source"`
				Destination string `json:"Destination"`
			}{mount("volume", "v", "/var/lib/docker/volumes/v/_data", "/data")},
		}
		got := describeContainer(api, "/host")
		if got.Kind != c.wantKind || got.Engine != c.wantEngine {
			t.Errorf("%s: kind/engine = %q/%q, want %q/%q",
				c.image, got.Kind, got.Engine, c.wantKind, c.wantEngine)
		}
	}
}

func TestDescribeContainerStatelessAndSocketSkipped(t *testing.T) {
	// No mounts at all: nothing to back up.
	got := describeContainer(dockerAPIContainer{Names: []string{"/glances"}, Image: "nicolargo/glances"}, "/host")
	if got.Kind != proto.DockerKindStateless {
		t.Errorf("mountless container kind = %q, want stateless", got.Kind)
	}

	// The Docker socket is a control channel, not data, and must never be
	// offered as a backup path.
	api := dockerAPIContainer{
		Names: []string{"/portainer"},
		Image: "portainer/portainer-ce:latest",
		Mounts: []struct {
			Type        string `json:"Type"`
			Name        string `json:"Name"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		}{
			mount("bind", "", "/var/run/docker.sock", "/var/run/docker.sock"),
			mount("volume", "portainer_data", "/var/lib/docker/volumes/portainer_data/_data", "/data"),
		},
	}
	got = describeContainer(api, "/host")
	if len(got.Mounts) != 1 || got.Mounts[0].Destination != "/data" {
		t.Fatalf("mounts = %+v, want only the data volume", got.Mounts)
	}
	if got.Mounts[0].BackupPath != "/host/var/lib/docker/volumes/portainer_data/_data" {
		t.Errorf("backup path = %q, want the /host-prefixed path", got.Mounts[0].BackupPath)
	}
}

func TestPersistenceNoteFlagsUnmountedDataDir(t *testing.T) {
	// Portainer keeps its database in /data. A volume mounted somewhere else
	// (a real misconfiguration seen in the wild) means nothing is persisted,
	// and a backup of that volume would silently capture an empty directory.
	api := dockerAPIContainer{
		Names: []string{"/portainer"},
		Image: "portainer/portainer-ce:latest",
		Mounts: []struct {
			Type        string `json:"Type"`
			Name        string `json:"Name"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		}{mount("volume", "portainer_data", "/var/lib/docker/volumes/portainer_data/_data", "/dataviz")},
	}
	if note := describeContainer(api, "/host").Note; note == "" {
		t.Error("expected a note when the data directory has no mount covering it")
	}

	// Correctly mounted at /data: no note.
	api.Mounts[0] = mount("volume", "portainer_data", "/var/lib/docker/volumes/portainer_data/_data", "/data")
	if note := describeContainer(api, "/host").Note; note != "" {
		t.Errorf("unexpected note for a correctly mounted container: %s", note)
	}

	// A mount above the data directory also covers it.
	api.Image = "mysql:8.4"
	api.Mounts[0] = mount("volume", "v", "/var/lib/docker/volumes/v/_data", "/var/lib")
	if note := describeContainer(api, "/host").Note; note != "" {
		t.Errorf("a parent mount should count as covering the data dir, got: %s", note)
	}
}

// serveFakeDocker starts a Docker-like API on a unix socket and points the
// agent at it. It rejects versioned request paths the way Docker 25+ does,
// so pinning an outdated API version fails the test rather than silently
// working against a permissive double.
func serveFakeDocker(t *testing.T, body string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v") {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"message":"client version is too old. Minimum supported API version is 1.44"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	old := dockerSocket
	dockerSocket = sock
	t.Cleanup(func() { dockerSocket = old })
}

func TestDockerInventoryUsesUnversionedAPI(t *testing.T) {
	serveFakeDocker(t, `[
	  {"Names":["/db"],"Image":"mysql:8.4","State":"running",
	   "Labels":{"com.docker.compose.project":"app"},
	   "Mounts":[{"Type":"volume","Name":"d","Source":"/var/lib/docker/volumes/d/_data","Destination":"/var/lib/mysql"}]}
	]`)

	inv := dockerInventory(t.Context())
	if !inv.Available {
		t.Fatalf("inventory unavailable: %s", inv.Error)
	}
	if len(inv.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(inv.Containers))
	}
	c := inv.Containers[0]
	if c.Name != "db" || c.Engine != "mysql" || c.Stack != "app" {
		t.Errorf("container = %+v, want the mysql container in stack 'app'", c)
	}
}

// A daemon error must surface as a message, not as an empty list that the
// GUI would render as "this host has no containers".
func TestDockerInventoryReportsAPIError(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"message":"something went wrong"}`)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	old := dockerSocket
	dockerSocket = sock
	t.Cleanup(func() { dockerSocket = old })

	inv := dockerInventory(t.Context())
	if inv.Available {
		t.Error("Available should be false when the daemon returned an error")
	}
	if !strings.Contains(inv.Error, "something went wrong") {
		t.Errorf("Error = %q, want it to carry the daemon's message", inv.Error)
	}
}

func TestDockerInventoryWithoutSocket(t *testing.T) {
	// Not a Docker host: reported as unavailable rather than an error, so
	// the GUI simply omits the picker.
	inv := dockerInventory(t.Context())
	if inv.Available && len(inv.Containers) == 0 {
		t.Error("Available should be false when no containers were read")
	}
}

func TestDescribeContainerSkipsHostRootBind(t *testing.T) {
	// The backup agent mounts / at /host so it can read the host's data.
	// Offering that as a tick-box would mean "back up the entire
	// filesystem", which walks /proc, /sys and /dev and buries the run in
	// permission errors.
	api := dockerAPIContainer{
		Names: []string{"/backup-agent"},
		Image: "centralbackup/agent",
		Mounts: []struct {
			Type        string `json:"Type"`
			Name        string `json:"Name"`
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		}{
			mount("volume", "d", "/var/lib/docker/volumes/deploy_backup-agent-data/_data", "/var/lib/backup-agent"),
			mount("bind", "", "/var/run/docker.sock", "/var/run/docker.sock"),
			mount("bind", "", "/", "/host"),
		},
	}
	got := describeContainer(api, "/host")
	for _, m := range got.Mounts {
		if m.Source == "/" || m.BackupPath == "/host" {
			t.Fatalf("host-root bind was offered as a backup path: %+v", m)
		}
	}
	if len(got.Mounts) != 1 || got.Mounts[0].Destination != "/var/lib/backup-agent" {
		t.Errorf("mounts = %+v, want only the agent's own data volume", got.Mounts)
	}
}

func TestIsPseudoFS(t *testing.T) {
	if !isPseudoFS("/proc") {
		t.Error("/proc should be recognised as a kernel filesystem")
	}
	if !isPseudoFS("/sys") {
		t.Error("/sys should be recognised as a kernel filesystem")
	}
	// An ordinary directory — including one merely named "proc" — must not
	// be skipped: the check is by filesystem type, not by name.
	dir := t.TempDir()
	if isPseudoFS(dir) {
		t.Error("a temp dir must not be treated as a kernel filesystem")
	}
	named := filepath.Join(dir, "proc")
	if err := os.MkdirAll(named, 0o755); err != nil {
		t.Fatal(err)
	}
	if isPseudoFS(named) {
		t.Error("a directory named 'proc' on a real filesystem must not be skipped")
	}
}
