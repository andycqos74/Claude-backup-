package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	"centralbackup/internal/oauthfwd"
	"centralbackup/internal/server/storage"
)

// TestCentralCallbackEndToEnd drives a tenant's whole "connect OneDrive"
// flow through the real central forwarder:
//
//	connect -> authorize URL -> forwarder -> tenant callback -> token exchange
//
// and asserts the thing that silently breaks this design if it is wrong:
// the redirect_uri presented at the token exchange must be the central
// callback, byte-identical to the one in the authorization request. A
// mismatch here is what Microsoft answers with a bare invalid_grant.
func TestCentralCallbackEndToEnd(t *testing.T) {
	const centralCallback = "https://connect.example.com/oauth/callback"

	var mu sync.Mutex
	var exchangeRedirectURI string

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			r.ParseForm()
			mu.Lock()
			exchangeRedirectURI = r.FormValue("redirect_uri")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "fake-access", "refresh_token": "fake-refresh",
				"token_type": "Bearer", "expires_in": 3600,
			})
		case strings.HasSuffix(r.URL.Path, "/me"):
			json.NewEncoder(w).Encode(map[string]any{"userPrincipalName": "admin@contoso.com"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()

	orig := providerOAuthConfig[storage.ProviderOneDrive]
	patched := orig
	patched.Endpoint = oauth2.Endpoint{AuthURL: fake.URL + "/authorize", TokenURL: fake.URL + "/token"}
	providerOAuthConfig[storage.ProviderOneDrive] = patched
	defer func() { providerOAuthConfig[storage.ProviderOneDrive] = orig }()

	origAcct := accountInfoEndpoint[storage.ProviderOneDrive]
	accountInfoEndpoint[storage.ProviderOneDrive] = struct{ URL, Field string }{fake.URL + "/me", "userPrincipalName"}
	defer func() { accountInfoEndpoint[storage.ProviderOneDrive] = origAcct }()

	s, sess := oauthTestServer(t)
	s.cfg.OAuthCallbackURL = centralCallback
	s.cfg.TenantSlug = "acme"
	s.cfg.GUIURL = "https://acme.gui.example.com"

	// 1. The operator's app credentials, seeded by the provisioner rather
	//    than typed by the customer.
	req := httptest.NewRequest("POST", "/api/admin/storage/app", strings.NewReader(
		`{"provider":"onedrive","client_id":"operator-cid","client_secret":"operator-secret","folder":"cb"}`))
	req.Header.Set("X-Requested-With", "fetch")
	withSession(req, sess)
	rec := httptest.NewRecorder()
	s.adminAuth(s.handleStorageSetApp)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("save app: %d %s", rec.Code, rec.Body)
	}
	var appResp struct {
		RedirectURL string `json:"redirect_url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &appResp)
	if appResp.RedirectURL != centralCallback {
		t.Errorf("GUI shows redirect URL %q, want the central callback", appResp.RedirectURL)
	}

	// 2. Connect: the authorize URL must carry the central redirect_uri and
	//    a state naming this tenant.
	req = httptest.NewRequest("POST", "/api/admin/storage/connect", nil)
	req.Header.Set("X-Requested-With", "fetch")
	withSession(req, sess)
	rec = httptest.NewRecorder()
	s.adminAuth(s.handleStorageConnect)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	var connectResp struct {
		AuthURL string `json:"auth_url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &connectResp)
	authURL, err := url.Parse(connectResp.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := authURL.Query().Get("redirect_uri"); got != centralCallback {
		t.Fatalf("authorize redirect_uri = %q, want %q", got, centralCallback)
	}
	state := authURL.Query().Get("state")
	if !strings.HasPrefix(state, "acme.") {
		t.Fatalf("state = %q, want it to name tenant acme", state)
	}

	// 3. The provider sends the browser to the CENTRAL callback. The real
	//    forwarder decides where it goes next.
	fwd, err := oauthfwd.New("https://{slug}.gui.example.com", oauthfwd.DefaultTenantPath, "acme,globex")
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	fwd.HandleCallback(rec, httptest.NewRequest("GET",
		"/oauth/callback?code=authcode&state="+url.QueryEscape(state), nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("forwarder status = %d, want 302: %s", rec.Code, rec.Body)
	}
	forwarded, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if forwarded.Host != "acme.gui.example.com" {
		t.Fatalf("forwarded to %q, want the acme tenant", forwarded.Host)
	}
	if forwarded.Path != oauthfwd.DefaultTenantPath {
		t.Fatalf("forwarded to path %q", forwarded.Path)
	}

	// 4. The browser lands on the tenant, which exchanges the code itself.
	req = httptest.NewRequest("GET", forwarded.Path+"?"+forwarded.RawQuery, nil)
	withSession(req, sess)
	rec = httptest.NewRecorder()
	s.handleStorageOAuthCallback(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("tenant callback = %d, want 302: %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "storage=connected") {
		t.Fatalf("tenant callback redirected to %q, expected success", loc)
	}

	// 5. The assertion this whole test exists for.
	mu.Lock()
	got := exchangeRedirectURI
	mu.Unlock()
	if got != centralCallback {
		t.Errorf("token exchange sent redirect_uri = %q, want %q\n"+
			"(a mismatch here is what providers reject with invalid_grant)", got, centralCallback)
	}

	// And storage is actually connected.
	cfg := s.loadStorageConfig()
	if cfg.Provider != storage.ProviderOneDrive || cfg.RefreshToken != "fake-refresh" {
		t.Errorf("storage not connected: provider=%s refresh=%q", cfg.Provider, cfg.RefreshToken)
	}
	if cfg.Account != "admin@contoso.com" {
		t.Errorf("account label = %q", cfg.Account)
	}
}
