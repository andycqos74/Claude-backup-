package server

import (
	"net/http/httptest"
	"os"
	"testing"
)

func TestNormalizePublicURL(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""},
		{"https://backup.example.com:8443", "https://backup.example.com:8443"},
		{"https://backup.example.com:8443/", "https://backup.example.com:8443"},
		{"  https://backup.example.com:8443  ", "https://backup.example.com:8443"},
		{"https://backup.example.com", "https://backup.example.com"},
		// A bare host:port is accepted and assumed https.
		{"backup.example.com:8443", "https://backup.example.com:8443"},
		{"192.0.2.10:8443", "https://192.0.2.10:8443"},
	}
	for _, tc := range ok {
		got, err := normalizePublicURL(tc.in)
		if err != nil {
			t.Errorf("normalizePublicURL(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizePublicURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"http://backup.example.com:8443", // the server has no plain-HTTP listener
		"wss://backup.example.com:8443",  // wrong scheme entirely
		"https://backup.example.com/sub", // a path would corrupt the callback URL
		"https://",                       // no host
	}
	for _, in := range bad {
		if got, err := normalizePublicURL(in); err == nil {
			t.Errorf("normalizePublicURL(%q) = %q, want an error", in, got)
		}
	}
}

func TestPublicBaseURLPrefersConfigOverRequestHost(t *testing.T) {
	s := &Server{cfg: Config{
		Listen:     ":8443",
		ServerName: "backup.example.com",
		PublicURL:  "https://backup.example.com:8443",
	}}

	// Browsing by IP must not leak that IP into enrollment commands or the
	// OAuth redirect — this is the whole point of the setting.
	r := httptest.NewRequest("POST", "/api/admin/tokens", nil)
	r.Host = "192.0.2.10:8443"
	if got := s.publicBaseURL(r); got != "https://backup.example.com:8443" {
		t.Errorf("publicBaseURL with CB_PUBLIC_URL set = %q, want the configured origin", got)
	}
	want := "https://backup.example.com:8443" + oauthCallbackPath
	if got := s.oauthRedirectURLFromRequest(r, ""); got != want {
		t.Errorf("oauthRedirectURLFromRequest = %q, want %q", got, want)
	}
	if got := s.oauthRedirectURL(); got != want {
		t.Errorf("oauthRedirectURL (no request) = %q, want %q", got, want)
	}
}

func TestPublicBaseURLFallsBackToRequestHost(t *testing.T) {
	s := &Server{cfg: Config{Listen: ":8443", ServerName: "backup.example.com"}}

	r := httptest.NewRequest("POST", "/api/admin/tokens", nil)
	r.Host = "other.example.com:8443"
	if got := s.publicBaseURL(r); got != "https://other.example.com:8443" {
		t.Errorf("publicBaseURL without CB_PUBLIC_URL = %q, want the request host", got)
	}

	// With no request at all (background token refresh) it falls back to
	// CB_SERVER_NAME plus the listen port.
	if got := s.publicBaseURL(nil); got != "https://backup.example.com:8443" {
		t.Errorf("publicBaseURL(nil) = %q, want the CB_SERVER_NAME origin", got)
	}
}

func TestNewRejectsInvalidPublicURL(t *testing.T) {
	// Must fail before creating the data directory, so the typo is obvious
	// at startup rather than at enrollment time.
	dir := t.TempDir() + "/never-created"
	_, err := New(Config{DataDir: dir, Listen: ":8443", PublicURL: "http://nope.example.com"})
	if err == nil {
		t.Fatal("New accepted an http:// CB_PUBLIC_URL, want an error")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("New created %s before validating CB_PUBLIC_URL", dir)
	}
}
