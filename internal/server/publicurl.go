package server

import (
	"context"
	"fmt"
	"net"
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

// publicBaseURL returns the origin *agents* must use to reach this server,
// with no trailing slash. It is the base for enrollment commands, and the
// default for the OAuth callback when no separate GUI address is configured.
//
// A configured CB_PUBLIC_URL always wins. Deriving these values from the
// request means that browsing the GUI by IP (or by any alternate name)
// silently mints enrollment commands and a redirect URI pointing at that
// address: the agent then fails certificate-fingerprint verification, or
// the provider rejects the unregistered redirect URI. Both failures surface
// far from their cause, so pinning the value is worth the extra setting.
//
// Requests that arrived on the GUI listener carry the proxy's hostname —
// the tunnel address, on which TLS is terminated by something that is not
// this server. An agent pointed there can never match the pinned
// fingerprint, so that Host is never used here.
func (s *Server) publicBaseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	if r != nil && r.Host != "" && !fromGUIListener(r) {
		return "https://" + r.Host
	}
	return s.staticPublicBaseURL()
}

// browserBaseURL returns the origin the *operator's browser* uses, with no
// trailing slash. This is what the storage OAuth redirect must use: consent
// happens in the browser, so the provider has to send the user back to the
// address the browser knows — behind a tunnel, not the agents' address.
//
// CB_GUI_URL wins when the GUI is served through a proxy; otherwise this is
// exactly publicBaseURL, so an un-proxied deployment keeps pinning the
// redirect to CB_PUBLIC_URL rather than to whatever host was browsed.
func (s *Server) browserBaseURL(r *http.Request) string {
	if s.cfg.GUIURL != "" {
		return s.cfg.GUIURL
	}
	return s.publicBaseURL(r)
}

// guiListenerCtxKey marks a request as having arrived on the plain-HTTP GUI
// listener, i.e. from a TLS-terminating proxy rather than from the network
// directly.
type guiListenerCtxKey struct{}

// markGUIListener tags every request it passes through, so URL construction
// can tell a proxied GUI request from a direct one.
func markGUIListener(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), guiListenerCtxKey{}, true)))
	})
}

func fromGUIListener(r *http.Request) bool {
	v, _ := r.Context().Value(guiListenerCtxKey{}).(bool)
	return v
}

// normalizeGUIURL validates and canonicalises CB_GUI_URL — the address the
// operator's browser uses for the GUI when it is served through a proxy
// (e.g. "https://backup.example.com"). Empty means "derive it from the
// incoming request".
//
// https is required because the session cookie is issued with the Secure
// attribute: over a plain-HTTP origin the browser discards it and login
// silently never sticks. http://localhost is the one exception, since
// browsers treat loopback as a secure context — useful for testing a proxy
// setup before DNS exists.
func normalizeGUIURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("CB_GUI_URL is not a valid URL: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("CB_GUI_URL has no host: %q", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("CB_GUI_URL must be a bare origin with no path (got %q): "+
			"serve the GUI at the root of its hostname", u.Path)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Host) {
			return "", fmt.Errorf("CB_GUI_URL must use https (got %q): the admin session "+
				"cookie is Secure-only, so a plain-HTTP GUI address can never sign in", raw)
		}
	default:
		return "", fmt.Errorf("CB_GUI_URL must use https (got scheme %q)", u.Scheme)
	}
	return u.Scheme + "://" + u.Host, nil
}

// isLoopbackHost reports whether a URL host is localhost or a loopback IP.
func isLoopbackHost(host string) bool {
	h := host
	if name, _, err := net.SplitHostPort(host); err == nil {
		h = name
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
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
