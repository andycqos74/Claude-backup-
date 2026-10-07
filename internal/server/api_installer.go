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
	// 32-bit Windows keeps its own filename so an admin who downloads both
	// cannot install the x86 build on an x64 machine by mistake, where WOW64
	// would silently redirect System32 reads to SysWOW64.
	"windows-386": {"backup-agent-windows-386.exe", "backup-agent-installer-x86.exe"},
	// Legacy build for Windows 7 SP1 / Embedded POSReady 7 (NT 6.1), which
	// the normal Go 1.25 binaries cannot run on. Built by
	// scripts/build-legacy-agent.sh with Go 1.20.
	"windows-386-legacy": {"backup-agent-windows-386-legacy.exe", "backup-agent-installer-x86-win7.exe"},
	"linux-amd64":        {"backup-agent-linux-amd64", "backup-agent-installer"},
	"linux-arm64":        {"backup-agent-linux-arm64", "backup-agent-installer"},
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
