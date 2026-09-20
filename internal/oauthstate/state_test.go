package oauthstate

import "testing"

func TestEncodeAndTenantRoundTrip(t *testing.T) {
	const nonce = "9f8e7d6c5b4a39281706"
	if got := Encode("acme", nonce); got != "acme."+nonce {
		t.Errorf("Encode = %q", got)
	}
	if got := Tenant(Encode("acme", nonce)); got != "acme" {
		t.Errorf("Tenant(Encode(...)) = %q, want acme", got)
	}
	// Single-tenant form: no slug, so the state is the bare nonce and names
	// no tenant.
	if got := Encode("", nonce); got != nonce {
		t.Errorf("Encode with no slug = %q, want the bare nonce", got)
	}
	if got := Tenant(nonce); got != "" {
		t.Errorf("Tenant(bare nonce) = %q, want empty", got)
	}
}

// The slug is substituted into a hostname, so anything that could break out
// of the tenant domain must be rejected. These are the inputs that would
// turn the forwarder into an open redirect.
func TestValidSlugRejectsHostnameEscapes(t *testing.T) {
	good := []string{"acme", "globex", "a1", "acme-corp", "a-b-c", "x0"}
	for _, s := range good {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}

	bad := []string{
		"",              // nothing
		"evil.com",      // a dot would climb out of the template domain
		"acme.evil",     //
		"a/b",           // path separator
		"a\\b",          // backslash
		"ACME",          // case: hostnames are lowercase here
		"-acme",         // leading hyphen is not a DNS label
		"acme-",         // trailing hyphen
		"acme corp",     // space
		"acme%2e",       // percent-escape
		"acme@evil.com", // userinfo trick
		"acme:8080",     // port
		"a#b",           // fragment
		"a?b",           // query
		"../acme",       // traversal
		string(make([]byte, 0)),
	}
	for _, s := range bad {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}

	long := ""
	for i := 0; i < maxSlugLen+1; i++ {
		long += "a"
	}
	if ValidSlug(long) {
		t.Error("ValidSlug accepted an over-long slug")
	}
}

// A state whose slug is invalid must name no tenant, so the caller refuses
// rather than falling back to a default.
func TestTenantRejectsUnsafeState(t *testing.T) {
	for _, state := range []string{
		"evil.com.nonce",
		".nonce",
		"-bad.nonce",
		"ACME.nonce",
		"a/b.nonce",
		"nonce-with-no-dot",
		"",
	} {
		if got := Tenant(state); got != "" {
			t.Errorf("Tenant(%q) = %q, want empty", state, got)
		}
	}
}
