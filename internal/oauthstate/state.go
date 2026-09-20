// Package oauthstate encodes which tenant an in-flight storage OAuth flow
// belongs to, so one centrally-registered redirect URI can serve every
// tenant.
//
// Providers cap how many redirect URIs one application may register
// (Microsoft: 100 for apps allowing personal accounts, 250 for work
// accounts, not raisable), so giving every tenant subdomain its own
// registered callback puts a hard ceiling on tenant count. The documented
// way around it is to register one URI and carry the destination in the
// OAuth `state` parameter — which is what this package encodes.
//
// State is "<slug>.<nonce>". The forwarder reads the slug to decide where to
// send the browser; the tenant server compares the *whole* string against
// what it stored, so the nonce's CSRF protection is unchanged.
package oauthstate

import "strings"

// maxSlugLen bounds a slug so it stays a plausible DNS label.
const maxSlugLen = 40

// Encode combines a tenant slug and a random nonce into a state parameter.
// An empty slug yields the bare nonce, which is the single-tenant form.
func Encode(slug, nonce string) string {
	if slug == "" {
		return nonce
	}
	return slug + "." + nonce
}

// Tenant returns the slug encoded in a state parameter, or "" if the state
// is not one this package produced. Callers must treat "" as "refuse this
// request" rather than as a default tenant.
//
// The format is exactly "<slug>.<nonce>": one dot, a valid slug before it,
// and a non-empty dotless nonce after. Encode never produces anything else
// (the nonce is hex), so anything else is malformed and is refused outright
// rather than partially interpreted — "evil.com.nonce" names no tenant
// rather than naming "evil".
func Tenant(state string) string {
	slug, nonce, ok := strings.Cut(state, ".")
	if !ok || nonce == "" || strings.Contains(nonce, ".") {
		return ""
	}
	if !ValidSlug(slug) {
		return ""
	}
	return slug
}

// ValidSlug reports whether s is safe to substitute into a hostname: lower
// case alphanumerics and inner hyphens only.
//
// This is the forwarder's only defence against being turned into an open
// redirect, so it is deliberately strict — no dots (which would let a slug
// climb out of the tenant domain), no slashes, no escapes, no upper case.
func ValidSlug(s string) bool {
	if len(s) == 0 || len(s) > maxSlugLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			// Not leading or trailing: "-x" and "x-" are not DNS labels.
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
