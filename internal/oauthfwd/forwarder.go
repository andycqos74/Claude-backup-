// Package oauthfwd implements the central storage-OAuth callback for a
// multi-tenant deployment: the single redirect URI registered with each
// cloud provider, whose only job is to send the browser on to the tenant
// that started the flow.
//
// See cmd/oauth-forwarder for the binary, and docs/central-oauth-callback.md
// for why a shared callback exists at all.
package oauthfwd

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"centralbackup/internal/oauthstate"
)

// DefaultTenantPath is the storage OAuth callback path on a tenant server.
const DefaultTenantPath = "/api/admin/storage/oauth/callback"

const slugPlaceholder = "{slug}"

// Forwarder routes a provider callback to the tenant named in its state.
type Forwarder struct {
	template   string          // e.g. https://{slug}.gui.example.com
	tenantPath string          // e.g. /api/admin/storage/oauth/callback
	allowlist  map[string]bool // nil means "any valid slug"
}

// New validates the configuration and builds a Forwarder. template must
// contain {slug} and be a bare https origin; allowlist is an optional
// comma-separated list of the only slugs that may be forwarded.
func New(template, tenantPath, allowlist string) (*Forwarder, error) {
	if !strings.Contains(template, slugPlaceholder) {
		return nil, fmt.Errorf("tenant template must contain %s, got %q", slugPlaceholder, template)
	}
	// Validate the shape with a stand-in slug: a template that is not
	// https, or that carries a path or query, would send authorization
	// codes somewhere unintended.
	u, err := url.Parse(strings.ReplaceAll(template, slugPlaceholder, "slug"))
	if err != nil {
		return nil, fmt.Errorf("tenant template is not a valid URL: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("tenant template must use https, got %q", template)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("tenant template has no host: %q", template)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("tenant template must be a bare origin (no path or query): %q", template)
	}
	if !strings.HasPrefix(tenantPath, "/") {
		return nil, fmt.Errorf("tenant path must start with /, got %q", tenantPath)
	}

	f := &Forwarder{template: template, tenantPath: tenantPath}
	if s := strings.TrimSpace(allowlist); s != "" {
		f.allowlist = map[string]bool{}
		for _, slug := range strings.Split(s, ",") {
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			if !oauthstate.ValidSlug(slug) {
				return nil, fmt.Errorf("allowlist contains an invalid slug: %q", slug)
			}
			f.allowlist[slug] = true
		}
		if len(f.allowlist) == 0 {
			return nil, fmt.Errorf("allowlist is set but lists no slugs")
		}
	}
	return f, nil
}

// Tenants returns how many slugs are allowlisted, and whether an allowlist
// is in force at all.
func (f *Forwarder) Tenants() (n int, enforced bool) {
	return len(f.allowlist), f.allowlist != nil
}

// target returns where a state parameter should be forwarded, or "" if it
// should be refused.
func (f *Forwarder) target(state string) string {
	slug := oauthstate.Tenant(state)
	if slug == "" {
		return ""
	}
	if f.allowlist != nil && !f.allowlist[slug] {
		return ""
	}
	return strings.ReplaceAll(f.template, slugPlaceholder, slug) + f.tenantPath
}

// HandleCallback forwards the provider's response to the tenant that started
// the flow. Provider errors are forwarded too, so the tenant's own GUI shows
// them; only an unroutable state is refused here.
func (f *Forwarder) HandleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	base := f.target(state)
	if base == "" {
		// Deliberately terse: the state is attacker-influenced, so it is
		// neither echoed to the browser nor logged.
		log.Printf("refused callback: state names no known tenant")
		http.Error(w, "This sign-in link is not valid. Start the connection again "+
			"from your backup server's Settings page.", http.StatusBadRequest)
		return
	}

	// Pass the query through untouched: code, state, and any provider extras
	// (error, error_description, session_state) all belong to the tenant.
	dest := base
	if q := r.URL.RawQuery; q != "" {
		dest += "?" + q
	}
	// Never log dest — it carries the authorization code.
	log.Printf("forwarding callback for tenant %q", oauthstate.Tenant(state))
	http.Redirect(w, r, dest, http.StatusFound)
}
