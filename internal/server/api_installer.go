package server

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"centralbackup/internal/bundle"
)

// Ready-to-run installers: the admin picks a platform and downloads an agent
// binary with this server's address, a fresh one-time token and the
// certificate fingerprint already inside it. Installing a client is then
// "download this file, run it" — no command to paste, and no bootstrap
// script whose shell or TLS stack can fail.

// installerTargets maps the platform chosen in the GUI to the prebuilt
// binary shipped in the server image, and the filename offered to the admin.
var installerTargets = map[string]struct{ file, download string }{
	"windows-amd64": {"backup-agent-windows-amd64.exe", "backup-agent-installer.exe"},
	"linux-amd64":   {"backup-agent-linux-amd64", "backup-agent-installer"},
	"linux-arm64":   {"backup-agent-linux-arm64", "backup-agent-installer"},
}

func (s *Server) handleInstaller(w http.ResponseWriter, r *http.Request) {
	target, ok := installerTargets[r.URL.Query().Get("platform")]
	if !ok {
		httpError(w, http.StatusBadRequest, "unknown platform")
		return
	}

	src := filepath.Join(s.cfg.AgentBins, target.file)
	f, err := os.Open(src)
	if err != nil {
		httpError(w, http.StatusNotFound,
			"no prebuilt agent for that platform in this server build")
		return
	}
	defer f.Close()

	// A fresh token per download: it is single-use, so a re-downloaded
	// installer must carry its own rather than sharing one.
	token, err := s.store.CreateEnrollToken("installer download", 24*time.Hour)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", target.download))
	// The body carries a live enrollment token; never let it be cached.
	w.Header().Set("Cache-Control", "no-store")

	if err := bundle.Append(w, f, bundle.Enrollment{
		ServerURL:   s.publicBaseURL(r),
		Token:       token,
		Fingerprint: s.fingerprint,
		Name:        strings.TrimSpace(r.URL.Query().Get("name")),
	}); err != nil {
		// The response is already partly written, so the download will fail
		// as a truncated file rather than as a clean error page.
		log.Printf("installer download for %s failed: %v", r.URL.Query().Get("platform"), err)
	}
}
