package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"centralbackup/internal/server/storage"
)

// Storage admin API: view/set the active backend and run the OAuth
// "connect" flow for cloud providers. Secrets (client secret, refresh
// token) are never returned to the browser.

func (s *Server) registerStorageAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/storage", s.adminAuth(s.handleStorageGet))
	mux.HandleFunc("POST /api/admin/storage/local", s.adminAuth(s.handleStorageUseLocal))
	mux.HandleFunc("POST /api/admin/storage/s3", s.adminAuth(s.handleStorageUseS3))
	mux.HandleFunc("POST /api/admin/storage/app", s.adminAuth(s.handleStorageSetApp))
	mux.HandleFunc("POST /api/admin/storage/connect", s.adminAuth(s.handleStorageConnect))
	// The OAuth provider redirects the browser here (a top-level GET, so it
	// carries the session cookie but not our X-Requested-With header; it is
	// therefore guarded by session + the one-time state parameter, not
	// adminAuth).
	mux.HandleFunc("GET /api/admin/storage/oauth/callback", s.handleStorageOAuthCallback)
}

type storageStatusJSON struct {
	Provider      string `json:"provider"`
	Connected     bool   `json:"connected"`
	Account       string `json:"account,omitempty"`
	Folder        string `json:"folder,omitempty"`
	HasApp        bool   `json:"has_app"` // client id/secret entered
	ClientID      string `json:"client_id,omitempty"`
	RedirectURL   string `json:"redirect_url"` // to paste into the provider console
	LocalDir      string `json:"local_dir,omitempty"`
	SupportsCloud bool   `json:"supports_cloud"`

	// S3 settings, echoed back to prefill the form. The secret key is
	// deliberately never returned.
	S3Endpoint  string `json:"s3_endpoint,omitempty"`
	S3Region    string `json:"s3_region,omitempty"`
	S3Bucket    string `json:"s3_bucket,omitempty"`
	S3AccessKey string `json:"s3_access_key,omitempty"`
	HasS3Secret bool   `json:"has_s3_secret"`
}

func (s *Server) handleStorageGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.loadStorageConfig()
	writeJSON(w, http.StatusOK, storageStatusJSON{
		Provider:      string(cfg.Provider),
		Connected:     cfg.Connected(),
		Account:       storageAccountLabel(cfg),
		Folder:        cfg.Folder,
		HasApp:        cfg.ClientID != "" && cfg.ClientSecret != "",
		ClientID:      cfg.ClientID,
		RedirectURL:   s.oauthRedirectURLFromRequest(r, cfg.Provider),
		LocalDir:      cfg.LocalDir,
		SupportsCloud: true,
		S3Endpoint:    cfg.S3Endpoint,
		S3Region:      cfg.S3Region,
		S3Bucket:      cfg.S3Bucket,
		S3AccessKey:   cfg.S3AccessKey,
		HasS3Secret:   cfg.S3SecretKey != "",
	})
}

func (s *Server) handleStorageUseLocal(w http.ResponseWriter, r *http.Request) {
	cfg := storage.Config{Provider: storage.ProviderLocal, LocalDir: s.cfg.StorageDir}
	if err := s.applyStorageConfig(cfg); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleStorageUseS3 validates and activates an S3-compatible destination.
// Unlike the OAuth providers there is no connect step: the credentials are
// usable immediately, so the configuration is proved by a live round trip
// before it is saved — a typo in the key or bucket is reported here rather
// than surfacing as a failed backup hours later.
func (s *Server) handleStorageUseS3(w http.ResponseWriter, r *http.Request) {
	req, err := decodeBody[struct {
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		Bucket    string `json:"bucket"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
		Folder    string `json:"folder"`
	}](r)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad request body")
		return
	}

	cur := s.loadStorageConfig()
	cfg := storage.Config{
		Provider:    storage.ProviderS3,
		S3Endpoint:  strings.TrimSpace(req.Endpoint),
		S3Region:    strings.TrimSpace(req.Region),
		S3Bucket:    strings.TrimSpace(req.Bucket),
		S3AccessKey: strings.TrimSpace(req.AccessKey),
		S3SecretKey: strings.TrimSpace(req.SecretKey),
		Folder:      strings.Trim(strings.TrimSpace(req.Folder), "/"),
	}
	// An empty secret means "keep the stored one", so the GUI never has to
	// echo it back to the browser.
	if cfg.S3SecretKey == "" && cur.Provider == storage.ProviderS3 {
		cfg.S3SecretKey = cur.S3SecretKey
	}

	backend, err := storage.NewS3(cfg)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := storageSelfTest(backend); err != nil {
		httpError(w, http.StatusBadGateway, "could not write to the bucket: "+err.Error())
		return
	}
	if err := s.applyStorageConfig(cfg); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// storageAccountLabel is the human-readable destination shown in the GUI.
// S3 has no account to name, so its bucket serves the same purpose.
func storageAccountLabel(cfg storage.Config) string {
	if cfg.Provider == storage.ProviderS3 {
		return cfg.S3Bucket
	}
	return cfg.Account
}

// storageSelfTest proves a backend can actually be written to, read back
// from and deleted, using a throwaway key.
func storageSelfTest(b storage.Backend) error {
	key := "healthcheck/" + newOAuthState()
	payload := []byte("central-backup connectivity check")
	if _, err := b.Put(key, bytes.NewReader(payload)); err != nil {
		return err
	}
	defer b.Delete(key)

	rc, err := b.Get(key)
	if err != nil {
		return err
	}
	defer rc.Close()
	got, err := io.ReadAll(io.LimitReader(rc, int64(len(payload))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("data read back did not match what was written")
	}
	return nil
}

// handleStorageSetApp stores the provider + OAuth app credentials (client
// id/secret + folder) without connecting yet. Connecting is a separate
// step so the admin can register the redirect URI in the provider console
// first.
func (s *Server) handleStorageSetApp(w http.ResponseWriter, r *http.Request) {
	req, err := decodeBody[struct {
		Provider     string `json:"provider"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Folder       string `json:"folder"`
	}](r)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad request body")
		return
	}
	provider := storage.Provider(req.Provider)
	if _, ok := providerOAuthConfig[provider]; !ok {
		httpError(w, http.StatusBadRequest, "unknown or non-cloud provider")
		return
	}
	if strings.TrimSpace(req.ClientID) == "" || strings.TrimSpace(req.ClientSecret) == "" {
		httpError(w, http.StatusBadRequest, "client ID and secret are required")
		return
	}
	if msg := clientIDMismatch(provider, strings.TrimSpace(req.ClientID)); msg != "" {
		httpError(w, http.StatusBadRequest, msg)
		return
	}

	// Preserve an existing refresh token only if the app identity is
	// unchanged; otherwise the old token no longer applies.
	cur := s.loadStorageConfig()
	cfg := storage.Config{
		Provider:     provider,
		ClientID:     strings.TrimSpace(req.ClientID),
		ClientSecret: strings.TrimSpace(req.ClientSecret),
		Folder:       strings.TrimSpace(req.Folder),
	}
	if cur.Provider == provider && cur.ClientID == cfg.ClientID && cur.ClientSecret == cfg.ClientSecret {
		cfg.RefreshToken = cur.RefreshToken
		cfg.Account = cur.Account
	}
	// Persist without activating: an unconnected cloud config can't build a
	// backend, and we don't want to disturb the running one until connected.
	if err := s.saveStorageConfig(cfg); err != nil {
		httpError(w, http.StatusInternalServerError, "could not save configuration")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"redirect_url": s.oauthRedirectURLFromRequest(r, provider),
	})
}

// handleStorageConnect begins the OAuth consent flow: it returns the
// provider authorization URL for the browser to visit.
func (s *Server) handleStorageConnect(w http.ResponseWriter, r *http.Request) {
	cfg := s.loadStorageConfig()
	if _, ok := providerOAuthConfig[cfg.Provider]; !ok {
		httpError(w, http.StatusBadRequest, "configure a cloud provider first")
		return
	}
	oc, err := oauthConfig(cfg, s.oauthRedirectURLFromRequest(r, cfg.Provider))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	state := newOAuthState()
	s.oauth.mu.Lock()
	s.oauth.state = state
	s.oauth.pending = cfg
	s.oauth.mu.Unlock()

	opts := append([]oauth2.AuthCodeOption{}, providerOAuthConfig[cfg.Provider].AuthParams...)
	authURL := oc.AuthCodeURL(state, opts...)
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": authURL})
}

// handleStorageOAuthCallback completes the flow: exchanges the code for
// tokens, stores the refresh token, activates the backend and redirects
// back to the Settings page.
func (s *Server) handleStorageOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.sessionUser(r) == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		// error_description carries the provider's actual reason (e.g. an
		// AADSTS code from Microsoft), which is what makes this diagnosable.
		desc := q.Get("error_description")
		full := e
		if desc != "" {
			full = e + ": " + desc
		}
		log.Printf("storage OAuth authorization denied: %s", full)
		s.oauthRedirectResult(w, r, "authorization failed — "+full)
		return
	}
	code, state := q.Get("code"), q.Get("state")

	s.oauth.mu.Lock()
	want, pending := s.oauth.state, s.oauth.pending
	s.oauth.state = "" // one-time
	s.oauth.mu.Unlock()

	if state == "" || state != want {
		s.oauthRedirectResult(w, r, "storage authorization state mismatch; please try again")
		return
	}
	oc, err := oauthConfig(pending, s.oauthRedirectURLFromRequest(r, pending.Provider))
	if err != nil {
		s.oauthRedirectResult(w, r, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	tok, err := oc.Exchange(ctx, code)
	if err != nil {
		s.oauthRedirectResult(w, r, "token exchange failed: "+err.Error())
		return
	}
	if tok.RefreshToken == "" {
		s.oauthRedirectResult(w, r, "provider did not return a refresh token; ensure offline access is granted and try again")
		return
	}
	pending.RefreshToken = tok.RefreshToken
	pending.Account = s.fetchAccountLabel(ctx, pending, tok.AccessToken)

	if err := s.applyStorageConfig(pending); err != nil {
		s.oauthRedirectResult(w, r, "connected, but could not activate storage: "+err.Error())
		return
	}
	s.oauthRedirectResult(w, r, "")
}

func (s *Server) oauthRedirectResult(w http.ResponseWriter, r *http.Request, errMsg string) {
	if errMsg == "" {
		http.Redirect(w, r, "/settings?storage=connected", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/settings?storage_error="+urlQueryEscape(errMsg), http.StatusFound)
}

// accountInfoEndpoint maps a provider to the API that returns a
// human-readable account label. It is a var so tests can point it at a fake
// server.
var accountInfoEndpoint = map[storage.Provider]struct{ URL, Field string }{
	storage.ProviderOneDrive:    {"https://graph.microsoft.com/v1.0/me", "userPrincipalName"},
	storage.ProviderGoogleDrive: {"https://www.googleapis.com/oauth2/v2/userinfo", "email"},
	storage.ProviderBox:         {"https://api.box.com/2.0/users/me", "login"},
}

// fetchAccountLabel best-effort reads a human-readable account name for the
// GUI; failure is non-fatal.
func (s *Server) fetchAccountLabel(ctx context.Context, cfg storage.Config, accessToken string) string {
	ep, ok := accountInfoEndpoint[cfg.Provider]
	if !ok {
		return ""
	}
	url, field := ep.URL, ep.Field
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return ""
	}
	var m map[string]any
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&m); err != nil {
		return ""
	}
	if v, ok := m[field].(string); ok {
		return v
	}
	return ""
}

// oauthRedirectURLFromRequest is the callback URL to register with the
// provider: CB_PUBLIC_URL when configured, otherwise the request's own host.
func (s *Server) oauthRedirectURLFromRequest(r *http.Request, _ storage.Provider) string {
	return s.publicBaseURL(r) + oauthCallbackPath
}

// clientIDMismatch catches the common mistake of pasting one provider's
// OAuth client ID into another (e.g. a Google client ID for OneDrive). It
// returns a helpful message, or "" if the ID looks plausible.
func clientIDMismatch(provider storage.Provider, clientID string) string {
	isGoogle := strings.HasSuffix(clientID, ".apps.googleusercontent.com")
	switch provider {
	case storage.ProviderOneDrive:
		if isGoogle {
			return "That looks like a Google client ID (ends in .apps.googleusercontent.com). OneDrive needs the Azure app registration's Application (client) ID (a GUID)."
		}
	case storage.ProviderGoogleDrive:
		if !isGoogle {
			return "That doesn't look like a Google OAuth client ID (it should end in .apps.googleusercontent.com). Use the client ID from Google Cloud → Credentials."
		}
	}
	return ""
}

func newOAuthState() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func urlQueryEscape(s string) string {
	return url.QueryEscape(s)
}
