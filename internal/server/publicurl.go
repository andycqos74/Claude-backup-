package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// oauthCallbackPath is the storage OAuth redirect path, registered verbatim
// in the provider's developer console.
const oauthCallbackPath = "/api/admin/storage/oauth/callback"

// normalizePublicURL validates and canonicalises CB_PUBLIC_URL — the
// externally reachable origin of this server, e.g.
// "https://backup.example.com:8443". An empty value is valid and means
// "derive it from the incoming request".
func normalizePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	// A bare host:port is a natural thing to type; accept it rather than
	// failing on the missing scheme.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("CB_PUBLIC_URL is not a valid URL: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("CB_PUBLIC_URL must use https (got %q): agents pin the server "+
			"certificate and the server has no plain-HTTP listener", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("CB_PUBLIC_URL has no host: %q", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("CB_PUBLIC_URL must be a bare origin with no path (got %q)", u.Path)
	}
	return "https://" + u.Host, nil
}

// publicBaseURL returns this server's externally reachable origin, with no
// trailing slash. It is the base for enrollment commands and the OAuth
// callback — both of which must match what agents and identity providers
// actually see.
//
// A configured CB_PUBLIC_URL always wins. Deriving these values from the
// request means that browsing the GUI by IP (or by any alternate name)
// silently mints enrollment commands and a redirect URI pointing at that
// address: the agent then fails certificate-fingerprint verification, or
// the provider rejects the unregistered redirect URI. Both failures surface
// far from their cause, so pinning the value is worth the extra setting.
func (s *Server) publicBaseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	if r != nil && r.Host != "" {
		return "https://" + r.Host
	}
	return s.staticPublicBaseURL()
}

// staticPublicBaseURL is the request-free fallback, built from
// CB_SERVER_NAME and the listen port. Background work (OAuth token refresh)
// has no request to learn the host from.
func (s *Server) staticPublicBaseURL() string {
	host := s.cfg.ServerName
	if i := indexByte(host, ','); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		host = "localhost"
	}
	return "https://" + host + portSuffix(s.cfg.Listen)
}
