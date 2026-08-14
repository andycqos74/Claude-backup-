package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"centralbackup/internal/proto"
)

// Docker inventory: the agent enumerates the containers on its host so the
// server GUI can offer them as tick-boxes in the job editor instead of
// making the admin discover volume paths by hand.
//
// We speak the Docker Engine API over its unix socket directly rather than
// depending on the official client library: one GET of /containers/json
// returns everything needed (names, image, labels, mounts) and keeps the
// agent binary free of a large dependency tree.

// dockerSocket is the Engine API socket. A var, and overridable via
// CB_DOCKER_SOCKET, so a non-default socket path (or a test double) can be
// used without rebuilding.
var dockerSocket = envOr("CB_DOCKER_SOCKET", "/var/run/docker.sock")

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// hostRootCandidates are where a containerised agent typically finds the
// host filesystem. deploy/agent-docker-compose.yml mounts it at /host.
var hostRootCandidates = []string{"/host"}

func dockerHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", dockerSocket)
			},
		},
	}
}

// dockerAPIContainer is the subset of /containers/json we consume.
type dockerAPIContainer struct {
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

// dockerInventory builds the container list for the server. It never
// returns an error for "no Docker here" — that is reported as
// Available:false so the GUI can simply omit the picker.
func dockerInventory(ctx context.Context) proto.DockerInventory {
	inv := proto.DockerInventory{HostRoot: detectHostRoot()}

	if _, err := os.Stat(dockerSocket); err != nil {
		return inv // Available stays false: not a Docker host, or socket not mounted
	}

	// Deliberately unversioned. Pinning a version ties us to a window the
	// daemon still accepts: Docker 25 dropped everything below 1.44, so a
	// pinned /v1.41/ fails with "client version is too old" on current
	// hosts, while pinning something new breaks older ones. An unversioned
	// path means "this daemon's current API", which every release accepts,
	// and the handful of fields read below have been stable throughout.
	req, err := http.NewRequestWithContext(ctx, "GET",
		"http://docker/containers/json?all=1", nil)
	if err != nil {
		inv.Error = err.Error()
		return inv
	}
	res, err := dockerHTTPClient().Do(req)
	if err != nil {
		inv.Error = "could not reach the Docker socket: " + err.Error()
		return inv
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		inv.Error = fmt.Sprintf("Docker API returned %s: %s", res.Status, strings.TrimSpace(string(body)))
		return inv
	}

	var raw []dockerAPIContainer
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&raw); err != nil {
		inv.Error = "could not parse the Docker API response: " + err.Error()
		return inv
	}

	inv.Available = true
	for _, c := range raw {
		inv.Containers = append(inv.Containers, describeContainer(c, inv.HostRoot))
	}
	// Stable ordering: stack, then name, so the GUI list doesn't jump around.
	sort.Slice(inv.Containers, func(i, j int) bool {
		a, b := inv.Containers[i], inv.Containers[j]
		if a.Stack != b.Stack {
			return a.Stack < b.Stack
		}
		return a.Name < b.Name
	})
	return inv
}

// detectHostRoot returns the prefix under which the host filesystem is
// visible to this agent, or "" when the agent runs natively on the host
// (paths then need no translation).
func detectHostRoot() string {
	for _, root := range hostRootCandidates {
		// A host-root mount always contains the Docker state directory;
		// checking for it avoids mistaking an unrelated /host directory.
		if fi, err := os.Stat(path.Join(root, "var/lib/docker")); err == nil && fi.IsDir() {
			return root
		}
	}
	return ""
}

// backupPathFor translates a host path into the path this agent must use in
// a job. A containerised agent sees the host under hostRoot; a native agent
// uses the path unchanged.
func backupPathFor(hostPath, hostRoot string) string {
	if hostRoot == "" || hostPath == "" || !strings.HasPrefix(hostPath, "/") {
		return hostPath
	}
	return path.Join(hostRoot, hostPath)
}

// dbEngines maps an image name fragment to the database engine it runs.
// Matching is on the image reference, which is how these are recognisable
// without inspecting the container's contents.
var dbEngines = []struct{ match, engine string }{
	{"mysql", "mysql"},
	{"mariadb", "mysql"},
	{"percona", "mysql"},
	{"postgres", "postgres"},
	{"timescale", "postgres"},
	{"pgvector", "postgres"},
	{"mssql/server", "mssql"},
	{"sqlserver", "mssql"},
	{"mongo", "mongo"},
	{"redis", "redis"},
	{"valkey", "redis"},
}

// embeddedDBApps are images known to keep a live SQLite/BoltDB file in a
// volume: copyable, but only guaranteed consistent with a stop/start.
var embeddedDBApps = []string{
	"vaultwarden", "bitwarden", "n8n", "portainer", "gitea", "forgejo",
	"uptime-kuma", "home-assistant", "homeassistant", "jellyfin", "plex",
	"sonarr", "radarr", "lidarr", "prowlarr", "bazarr", "grafana",
	"nextcloud", "paperless", "immich", "miniflux", "wallabag",
}

// dataDirs are the in-container data directories of well-known images. A
// container whose data directory is not backed by a mount is not persisting
// its state at all, which is worth flagging loudly in the picker.
var dataDirs = map[string]string{
	"mysql":       "/var/lib/mysql",
	"mariadb":     "/var/lib/mysql",
	"postgres":    "/var/lib/postgresql/data",
	"mongo":       "/data/db",
	"portainer":   "/data",
	"vaultwarden": "/data",
	"gitea":       "/data",
	"grafana":     "/var/lib/grafana",
}

func describeContainer(c dockerAPIContainer, hostRoot string) proto.DockerContainer {
	name := ""
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	out := proto.DockerContainer{
		Name:    name,
		Image:   c.Image,
		Stack:   c.Labels["com.docker.compose.project"],
		Service: c.Labels["com.docker.compose.service"],
		State:   c.State,
	}
	for _, m := range c.Mounts {
		// Skip the Docker socket and other special files: they are not data.
		if m.Destination == "/var/run/docker.sock" {
			continue
		}
		out.Mounts = append(out.Mounts, proto.DockerMount{
			Type:        m.Type,
			Name:        m.Name,
			Source:      m.Source,
			Destination: m.Destination,
			BackupPath:  backupPathFor(m.Source, hostRoot),
		})
	}

	img := strings.ToLower(c.Image)
	for _, e := range dbEngines {
		if strings.Contains(img, e.match) {
			out.Kind, out.Engine = proto.DockerKindDatabase, e.engine
			break
		}
	}
	if out.Kind == "" {
		for _, app := range embeddedDBApps {
			if strings.Contains(img, app) {
				out.Kind = proto.DockerKindEmbedded
				break
			}
		}
	}
	if out.Kind == "" {
		if len(out.Mounts) == 0 {
			out.Kind = proto.DockerKindStateless
		} else {
			out.Kind = proto.DockerKindFiles
		}
	}
	if len(out.Mounts) == 0 && out.Kind != proto.DockerKindStateless {
		out.Kind = proto.DockerKindStateless
	}
	out.Note = persistenceNote(img, out.Mounts)
	return out
}

// persistenceNote flags a container whose known data directory has no mount
// covering it — its state lives in the container's writable layer and is
// destroyed whenever the container is recreated. Backing such a container up
// from its volumes would silently capture nothing.
func persistenceNote(image string, mounts []proto.DockerMount) string {
	for frag, dir := range dataDirs {
		if !strings.Contains(image, frag) {
			continue
		}
		for _, m := range mounts {
			// A mount at or above the data directory covers it.
			if m.Destination == dir || strings.HasPrefix(dir, strings.TrimSuffix(m.Destination, "/")+"/") {
				return ""
			}
		}
		return fmt.Sprintf("No mount covers %s, where this image stores its data — "+
			"it is not being persisted and will be lost when the container is recreated.", dir)
	}
	return ""
}

// handleDiscoverDocker answers a server DiscoverDocker request.
func (a *Agent) handleDiscoverDocker(cmd proto.DiscoverDocker) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inv := dockerInventory(ctx)
	inv.RequestID = cmd.RequestID
	if err := a.send(proto.MsgDockerInventory, inv); err != nil {
		// Nothing to retry against: the server times the request out and
		// tells the admin, which is a better outcome than a blocked GUI.
		return
	}
}
