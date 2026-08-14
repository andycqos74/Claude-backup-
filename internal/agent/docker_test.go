package agent

import (
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

func TestDockerInventoryWithoutSocket(t *testing.T) {
	// Not a Docker host: reported as unavailable rather than an error, so
	// the GUI simply omits the picker.
	inv := dockerInventory(t.Context())
	if inv.Available && len(inv.Containers) == 0 {
		t.Error("Available should be false when no containers were read")
	}
}
