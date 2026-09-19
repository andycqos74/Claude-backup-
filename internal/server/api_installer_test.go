package server

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"centralbackup/internal/bundle"
)

// writeFakeAgents creates a stub file for every prebuilt binary the
// installer endpoint knows about, so a download can be exercised without
// cross-compiling real agents in the test.
func writeFakeAgents(t *testing.T, dir string) {
	t.Helper()
	for platform, target := range installerTargets {
		body := fmt.Sprintf("fake agent for %s", platform)
		if err := os.WriteFile(filepath.Join(dir, target.file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Every advertised platform must serve its own binary with its own embedded
// enrollment. windows-386 was added alongside windows-amd64, so this also
// guards against the two sharing a file or a download name.
func TestInstallerServesEachPlatform(t *testing.T) {
	s := testServer(t)
	s.cfg.AgentBins = t.TempDir()
	s.fingerprint = "AA:BB:CC"
	writeFakeAgents(t, s.cfg.AgentBins)

	seenTokens := map[string]string{}
	for platform, target := range installerTargets {
		r := httptest.NewRequest("GET", "/api/admin/installer?platform="+platform+"&name=box", nil)
		r.Host = "backup.example.com:8443"
		w := httptest.NewRecorder()
		s.handleInstaller(w, r)

		if w.Code != 200 {
			t.Fatalf("%s: status %d, want 200", platform, w.Code)
		}
		if got, want := w.Header().Get("Content-Disposition"),
			fmt.Sprintf("attachment; filename=%q", target.download); got != want {
			t.Errorf("%s: Content-Disposition = %q, want %q", platform, got, want)
		}
		// A live token must never be cached by a proxy or the browser.
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", platform, got)
		}

		// The downloaded file must be that platform's binary with a
		// readable enrollment appended.
		path := filepath.Join(t.TempDir(), "dl")
		if err := os.WriteFile(path, w.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		e, ok, err := bundle.Read(path)
		if err != nil || !ok {
			t.Fatalf("%s: reading embedded enrollment: ok=%v err=%v", platform, ok, err)
		}
		if e.ServerURL != "https://backup.example.com:8443" {
			t.Errorf("%s: ServerURL = %q", platform, e.ServerURL)
		}
		if e.Fingerprint != "AA:BB:CC" || e.Name != "box" {
			t.Errorf("%s: enrollment = %+v", platform, e)
		}
		if want := fmt.Sprintf("fake agent for %s", platform); string(w.Body.Bytes()[:len(want)]) != want {
			t.Errorf("%s: served the wrong binary", platform)
		}

		// Tokens are single-use, so no two downloads may share one.
		if other, dup := seenTokens[e.Token]; dup {
			t.Errorf("%s and %s were issued the same token", platform, other)
		}
		seenTokens[e.Token] = platform
	}
}

// The x86 build must not be downloadable under the x64 name: installing it
// on a 64-bit host would put the agent under WOW64, where System32 reads
// are silently redirected to SysWOW64.
func TestInstallerWindowsBuildsAreDistinct(t *testing.T) {
	x64 := installerTargets["windows-amd64"]
	x86 := installerTargets["windows-386"]
	win7 := installerTargets["windows-386-legacy"]
	if x86.file == "" {
		t.Fatal("no windows-386 target registered")
	}
	if win7.file == "" {
		t.Fatal("no windows-386-legacy target registered")
	}
	// All three Windows builds must serve distinct binaries under distinct
	// download names, so none can be installed on the wrong OS/arch by
	// picking the wrong button.
	wins := []struct{ file, download string }{
		{x64.file, x64.download}, {x86.file, x86.download}, {win7.file, win7.download},
	}
	for i := range wins {
		for j := i + 1; j < len(wins); j++ {
			if wins[i].file == wins[j].file || wins[i].download == wins[j].download {
				t.Errorf("windows targets %+v and %+v must differ", wins[i], wins[j])
			}
		}
	}
}

func TestInstallerRejectsUnknownPlatform(t *testing.T) {
	s := testServer(t)
	s.cfg.AgentBins = t.TempDir()

	for _, platform := range []string{"", "freebsd-amd64", "windows-arm64", "../../etc/passwd"} {
		w := httptest.NewRecorder()
		s.handleInstaller(w, httptest.NewRequest("GET", "/api/admin/installer?platform="+platform, nil))
		if w.Code != 400 {
			t.Errorf("platform %q: status %d, want 400", platform, w.Code)
		}
	}
}

// A server image built before a platform existed has no binary for it; that
// must be a clean 404 rather than a truncated download.
func TestInstallerMissingBinary(t *testing.T) {
	s := testServer(t)
	s.cfg.AgentBins = t.TempDir() // empty

	w := httptest.NewRecorder()
	s.handleInstaller(w, httptest.NewRequest("GET", "/api/admin/installer?platform=windows-386", nil))
	if w.Code != 404 {
		t.Errorf("status %d, want 404", w.Code)
	}
}
