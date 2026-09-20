package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"centralbackup/internal/oauthstate"
)

func TestNormalizeOAuthCallbackURL(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""},
		{"https://connect.example.com/oauth/callback", "https://connect.example.com/oauth/callback"},
		{"  https://connect.example.com/oauth/callback  ", "https://connect.example.com/oauth/callback"},
		{"https://connect.example.com:8443/oauth/callback", "https://connect.example.com:8443/oauth/callback"},
	}
	for _, tc := range ok {
		got, err := normalizeOAuthCallbackURL(tc.in)
		if err != nil {
			t.Errorf("normalizeOAuthCallbackURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeOAuthCallbackURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"http://connect.example.com/oauth/callback", // providers reject plain HTTP
		"https://connect.example.com",               // no callback path
		"https://connect.example.com/",              // ditto
		"https://connect.example.com/cb?x=1",        // query would not match at exchange
		"https://connect.example.com/cb#frag",
		"https:///oauth/callback", // no host
	}
	for _, in := range bad {
		if got, err := normalizeOAuthCallbackURL(in); err == nil {
			t.Errorf("normalizeOAuthCallbackURL(%q) = %q, want an error", in, got)
		}
	}
}

// The redirect URI must be byte-identical on the authorization request, the
// token exchange and every later refresh, or the provider rejects it. All
// three read it from these two methods, so they must agree.
func TestCentralCallbackUsedEverywhere(t *testing.T) {
	const central = "https://connect.example.com/oauth/callback"
	s := &Server{cfg: Config{
		Listen:           ":8443",
		PublicURL:        "https://acme.backup.example.com:8443",
		GUIURL:           "https://acme.gui.example.com",
		OAuthCallbackURL: central,
		TenantSlug:       "acme",
	}}

	r := httptest.NewRequest("GET", "/api/admin/storage/oauth/start", nil)
	r.Host = "acme.gui.example.com"

	if got := s.oauthRedirectURLFromRequest(r, ""); got != central {
		t.Errorf("authorization/exchange redirect URI = %q, want the central callback", got)
	}
	// Background token refresh has no request and must still agree.
	if got := s.oauthRedirectURL(); got != central {
		t.Errorf("refresh redirect URI = %q, want the central callback", got)
	}
}

// Without the central callback configured, behaviour is exactly as before.
func TestWithoutCentralCallbackNothingChanges(t *testing.T) {
	s := &Server{cfg: Config{
		Listen:    ":8443",
		PublicURL: "https://backup.example.com:8443",
		GUIURL:    "https://gui.example.com",
	}}
	want := "https://gui.example.com" + oauthCallbackPath
	r := httptest.NewRequest("GET", "/x", nil)
	if got := s.oauthRedirectURLFromRequest(r, ""); got != want {
		t.Errorf("redirect URI = %q, want %q", got, want)
	}
	if got := s.oauthRedirectURL(); got != want {
		t.Errorf("refresh redirect URI = %q, want %q", got, want)
	}
}

// The state must carry the slug, or the forwarder cannot route the callback
// back to this tenant.
func TestConnectStateCarriesTenantSlug(t *testing.T) {
	state := oauthstate.Encode("acme", newOAuthState())
	if got := oauthstate.Tenant(state); got != "acme" {
		t.Errorf("Tenant(%q) = %q, want acme", state, got)
	}
	// And the nonce must still be there for the tenant's own CSRF check.
	if _, nonce, _ := strings.Cut(state, "."); len(nonce) < 32 {
		t.Errorf("nonce %q is shorter than expected", nonce)
	}
	// Single-tenant deployments keep a bare nonce.
	if got := oauthstate.Encode("", "abc"); got != "abc" {
		t.Errorf("Encode with no slug = %q", got)
	}
}

func TestNewValidatesCentralCallbackConfig(t *testing.T) {
	base := func() Config {
		dir := t.TempDir()
		return Config{DataDir: dir, StorageDir: dir + "/s", Listen: ":8443"}
	}

	// A central callback without a slug cannot be routed back.
	cfg := base()
	cfg.OAuthCallbackURL = "https://connect.example.com/oauth/callback"
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "CB_TENANT_SLUG") {
		t.Errorf("New without a slug: err = %v, want one naming CB_TENANT_SLUG", err)
	}

	// A slug that is not hostname-safe must be refused at startup, not when
	// a callback is forwarded.
	cfg = base()
	cfg.OAuthCallbackURL = "https://connect.example.com/oauth/callback"
	cfg.TenantSlug = "Acme.Corp"
	if _, err := New(cfg); err == nil {
		t.Error("New accepted an invalid CB_TENANT_SLUG")
	}

	// The valid combination starts.
	cfg = base()
	cfg.OAuthCallbackURL = "https://connect.example.com/oauth/callback"
	cfg.TenantSlug = "acme"
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New with valid central-callback config: %v", err)
	}
	srv.store.Close()
}
