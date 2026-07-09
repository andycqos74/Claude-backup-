package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

// oauthTestServer builds a Server with a real store and web UI, an admin
// session, and the local backend active — enough to drive the storage
// OAuth handlers.
func oauthTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	backend, _ := storage.NewLocalFS(filepath.Join(dir, "storage"))
	s := &Server{
		cfg:           Config{StorageDir: filepath.Join(dir, "storage"), Listen: ":8443"},
		store:         st,
		hub:           newHub(),
		storageActive: backend,
	}
	if err := st.CreateUser("admin", "testpass123"); err != nil {
		t.Fatal(err)
	}
	u, _ := st.Authenticate("admin", "testpass123")
	token, _ := st.CreateSession(u.ID, 3600_000_000_000)
	return s, token
}

func withSession(r *http.Request, token string) {
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
}

// TestStorageOAuthCallbackActivates drives the full connect->callback flow
// against a fake OAuth token endpoint and fake account endpoint, asserting
// the refresh token is persisted and the OneDrive backend becomes active.
func TestStorageOAuthCallbackActivates(t *testing.T) {
	// Fake Microsoft token + account server.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "fake-access",
				"refresh_token": "fake-refresh",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		case strings.HasSuffix(r.URL.Path, "/me"):
			json.NewEncoder(w).Encode(map[string]any{"userPrincipalName": "admin@contoso.com"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()

	// Point the OneDrive OAuth token URL and account endpoint at the fake.
	origProvider := providerOAuthConfig[storage.ProviderOneDrive]
	restored := origProvider
	restored.Endpoint = oauth2.Endpoint{AuthURL: fake.URL + "/authorize", TokenURL: fake.URL + "/token"}
	providerOAuthConfig[storage.ProviderOneDrive] = restored
	defer func() { providerOAuthConfig[storage.ProviderOneDrive] = origProvider }()

	origAcct := accountInfoEndpoint[storage.ProviderOneDrive]
	accountInfoEndpoint[storage.ProviderOneDrive] = struct{ URL, Field string }{fake.URL + "/me", "userPrincipalName"}
	defer func() { accountInfoEndpoint[storage.ProviderOneDrive] = origAcct }()

	s, sess := oauthTestServer(t)

	// 1. Save app credentials.
	body := `{"provider":"onedrive","client_id":"cid","client_secret":"csecret","folder":"cb"}`
	req := httptest.NewRequest("POST", "/api/admin/storage/app", strings.NewReader(body))
	req.Header.Set("X-Requested-With", "fetch")
	withSession(req, sess)
	rec := httptest.NewRecorder()
	s.adminAuth(s.handleStorageSetApp)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("save app: %d %s", rec.Code, rec.Body)
	}

	// 2. Connect -> get auth URL and capture the state the server stored.
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
	u, _ := url.Parse(connectResp.AuthURL)
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("no state in auth URL")
	}

	// 3. Provider redirects the browser back to the callback with code+state.
	cbURL := fmt.Sprintf("/api/admin/storage/oauth/callback?code=authcode&state=%s", state)
	req = httptest.NewRequest("GET", cbURL, nil)
	withSession(req, sess)
	rec = httptest.NewRecorder()
	s.handleStorageOAuthCallback(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "storage=connected") {
		t.Fatalf("callback redirected to %q, expected success", loc)
	}

	// Assert persisted config and active backend.
	cfg := s.loadStorageConfig()
	if cfg.Provider != storage.ProviderOneDrive {
		t.Fatalf("provider = %s, want onedrive", cfg.Provider)
	}
	if cfg.RefreshToken != "fake-refresh" {
		t.Fatalf("refresh token = %q, want fake-refresh", cfg.RefreshToken)
	}
	if cfg.Account != "admin@contoso.com" {
		t.Fatalf("account label = %q, want admin@contoso.com", cfg.Account)
	}
	if !cfg.Connected() {
		t.Fatal("config should report connected")
	}
}

func TestStorageOAuthCallbackStateMismatch(t *testing.T) {
	s, sess := oauthTestServer(t)
	s.oauth.state = "expected-state"
	req := httptest.NewRequest("GET", "/api/admin/storage/oauth/callback?code=x&state=wrong", nil)
	withSession(req, sess)
	rec := httptest.NewRecorder()
	s.handleStorageOAuthCallback(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "storage_error") {
		t.Fatalf("expected error redirect, got %q", rec.Header().Get("Location"))
	}
}
