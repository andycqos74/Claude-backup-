package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeGUIURL(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""},
		{"https://backup.example.com", "https://backup.example.com"},
		{"https://backup.example.com/", "https://backup.example.com"},
		{"  https://backup.example.com  ", "https://backup.example.com"},
		{"https://backup.example.com:8443", "https://backup.example.com:8443"},
		// A bare hostname is a natural thing to type; assume https.
		{"backup.example.com", "https://backup.example.com"},
		// Loopback is a secure context, so a Secure cookie survives there —
		// useful for testing a proxy before DNS exists.
		{"http://localhost:8080", "http://localhost:8080"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
	}
	for _, tc := range ok {
		got, err := normalizeGUIURL(tc.in)
		if err != nil {
			t.Errorf("normalizeGUIURL(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeGUIURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"http://backup.example.com",    // Secure session cookie would be dropped
		"wss://backup.example.com",     // wrong scheme entirely
		"https://backup.example.com/x", // a path would corrupt the callback URL
		"https://",                     // no host
	}
	for _, in := range bad {
		if got, err := normalizeGUIURL(in); err == nil {
			t.Errorf("normalizeGUIURL(%q) = %q, want an error", in, got)
		}
	}
}

// The GUI listener exists to sit behind a TLS-terminating proxy, so its Host
// header is the tunnel's hostname. Pointing an agent there guarantees a
// fingerprint mismatch, so enrollment must never derive its address from it.
func TestProxiedGUIRequestNeverSetsTheAgentAddress(t *testing.T) {
	s := &Server{cfg: Config{Listen: ":8443", ServerName: "backup.example.com"}}

	r := httptest.NewRequest("POST", "/api/admin/tokens", nil)
	r.Host = "gui.example.com" // what cloudflared passes through
	direct := s.publicBaseURL(r)
	if direct != "https://gui.example.com" {
		t.Fatalf("precondition: publicBaseURL on the direct listener = %q", direct)
	}

	proxied := markedGUIRequest(r)
	if got := s.publicBaseURL(proxied); got != "https://backup.example.com:8443" {
		t.Errorf("publicBaseURL for a proxied request = %q, want the CB_SERVER_NAME origin", got)
	}
}

// With the GUI proxied, the OAuth provider must redirect back to the address
// the browser is on, not to the agents' direct address.
func TestBrowserBaseURLPrefersGUIURL(t *testing.T) {
	s := &Server{cfg: Config{
		Listen:    ":8443",
		PublicURL: "https://backup.example.com:8443",
		GUIURL:    "https://gui.example.com",
	}}
	r := markedGUIRequest(httptest.NewRequest("GET", "/api/admin/storage/oauth/start", nil))

	if got := s.browserBaseURL(r); got != "https://gui.example.com" {
		t.Errorf("browserBaseURL = %q, want the GUI origin", got)
	}
	want := "https://gui.example.com" + oauthCallbackPath
	if got := s.oauthRedirectURLFromRequest(r, ""); got != want {
		t.Errorf("oauthRedirectURLFromRequest = %q, want %q", got, want)
	}
	// Background token refresh has no request and must agree, or the
	// provider rejects the refresh as a redirect-URI mismatch.
	if got := s.oauthRedirectURL(); got != want {
		t.Errorf("oauthRedirectURL (no request) = %q, want %q", got, want)
	}
	// Agents still get the direct address.
	if got := s.publicBaseURL(r); got != "https://backup.example.com:8443" {
		t.Errorf("publicBaseURL = %q, want the direct agent origin", got)
	}
}

// Agents pin this server's certificate, so the agent API must not be
// reachable through a proxy that terminates TLS — nor exposed to the
// internet by a tunnel that only needs to serve the GUI.
func TestGUIListenerOmitsAgentAPI(t *testing.T) {
	s := testServer(t)
	s.cfg = Config{AgentBins: t.TempDir()}
	ui, err := newWebUI(s)
	if err != nil {
		t.Fatal(err)
	}
	s.web = ui

	gui := s.guiHandler()

	agentPaths := []struct{ method, path string }{
		{"POST", "/api/agent/enroll"},
		{"GET", "/api/agent/ws"},
		{"POST", "/api/agent/blobs/check"},
		{"PUT", "/api/agent/blobs/abc"},
		{"GET", "/api/agent/blobs/abc"},
		{"POST", "/api/agent/snapshots"},
		{"GET", "/api/agent/manifests/m1"},
		{"GET", "/api/agent/transfers/t1/content"},
		{"PUT", "/api/agent/transfers/t1/content"},
	}
	for _, tc := range agentPaths {
		rec := httptest.NewRecorder()
		gui.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader("")))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the GUI listener = %d, want 404 (agent API must stay off the proxy)",
				tc.method, tc.path, rec.Code)
		}
	}

	// The GUI half is served: unauthenticated pages redirect to setup/login
	// rather than 404, and the admin API answers with 401.
	rec := httptest.NewRecorder()
	gui.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Code == http.StatusNotFound {
		t.Error("GET /login on the GUI listener 404ed; the GUI is not registered")
	}
	rec = httptest.NewRecorder()
	gui.ServeHTTP(rec, httptest.NewRequest("GET", "/api/admin/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/admin/overview unauthenticated = %d, want 401", rec.Code)
	}
}

func TestNewRequiresPublicURLWithGUIListener(t *testing.T) {
	dir := t.TempDir() + "/never-created"
	_, err := New(Config{DataDir: dir, Listen: ":8443", GUIListen: "127.0.0.1:8080"})
	if err == nil {
		t.Fatal("New accepted CB_GUI_LISTEN without CB_PUBLIC_URL, want an error")
	}
	if !strings.Contains(err.Error(), "CB_PUBLIC_URL") {
		t.Errorf("error %q does not name the missing setting", err)
	}

	_, err = New(Config{
		DataDir:   dir,
		Listen:    ":8443",
		GUIListen: "not-a-host-port",
		PublicURL: "https://backup.example.com:8443",
	})
	if err == nil {
		t.Fatal("New accepted a malformed CB_GUI_LISTEN, want an error")
	}
}

// markedGUIRequest returns r as the GUI listener's middleware would hand it
// to a handler.
func markedGUIRequest(r *http.Request) *http.Request {
	var got *http.Request
	markGUIListener(http.HandlerFunc(func(_ http.ResponseWriter, rr *http.Request) {
		got = rr
	})).ServeHTTP(httptest.NewRecorder(), r)
	return got
}

// ":8080" is what the Docker overlay uses (bind on all interfaces inside the
// container network); it must be accepted alongside "127.0.0.1:8080".
func TestGUIListenAcceptsBareColonPort(t *testing.T) {
	for _, addr := range []string{":8080", "127.0.0.1:8080", "0.0.0.0:8080"} {
		dir := t.TempDir()
		s, err := New(Config{
			DataDir:    dir,
			StorageDir: dir + "/storage",
			Listen:     ":8443",
			GUIListen:  addr,
			PublicURL:  "https://backup.example.com:8443",
		})
		if err != nil {
			t.Errorf("New with CB_GUI_LISTEN=%q: %v", addr, err)
			continue
		}
		s.store.Close()
	}
}
