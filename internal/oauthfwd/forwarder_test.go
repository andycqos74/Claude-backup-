package oauthfwd

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const tmpl = "https://{slug}.gui.example.com"

func newTestForwarder(t *testing.T, allowlist string) *Forwarder {
	t.Helper()
	f, err := New(tmpl, DefaultTenantPath, allowlist)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestForwardsToTheTenantInState(t *testing.T) {
	f := newTestForwarder(t, "")
	rec := httptest.NewRecorder()
	f.HandleCallback(rec, httptest.NewRequest("GET",
		"/oauth/callback?code=AUTHCODE&state=acme.abc123&session_state=xyz", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	want := "https://acme.gui.example.com" + DefaultTenantPath +
		"?code=AUTHCODE&state=acme.abc123&session_state=xyz"
	if loc != want {
		t.Errorf("Location = %q\n           want %q", loc, want)
	}
}

// Provider errors carry state too, and belong to the tenant's GUI so the
// admin sees the real reason (e.g. an AADSTS code).
func TestForwardsProviderErrors(t *testing.T) {
	f := newTestForwarder(t, "")
	rec := httptest.NewRecorder()
	f.HandleCallback(rec, httptest.NewRequest("GET",
		"/oauth/callback?error=access_denied&error_description=user+declined&state=globex.zzz", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://globex.gui.example.com") ||
		!strings.Contains(loc, "error=access_denied") {
		t.Errorf("Location = %q, want the error forwarded to globex", loc)
	}
}

// The invariant: whatever the state, any redirect this service emits stays
// inside the tenant template's domain. A valid-but-unknown slug forwarding
// to a subdomain of your own domain is acceptable (the tenant there rejects
// the nonce); a redirect to somebody else's domain never is.
func TestAnyRedirectStaysInsideTheTenantDomain(t *testing.T) {
	f := newTestForwarder(t, "")
	for _, state := range []string{
		"acme.n", "evil.com.nonce", "acme.evil.com.n", "//evil.com.n",
		"a%2fb.n", "acme@evil.com.n", "x.n", "", "..", "a.b.c.d",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/oauth/callback?code=C", nil)
		q := req.URL.Query()
		q.Set("state", state)
		req.URL.RawQuery = q.Encode()
		f.HandleCallback(rec, req)

		loc := rec.Header().Get("Location")
		if loc == "" {
			continue // refused outright, which is also fine
		}
		u, err := url.Parse(loc)
		if err != nil {
			t.Errorf("state %q produced an unparseable Location %q", state, loc)
			continue
		}
		if !strings.HasSuffix(u.Host, ".gui.example.com") {
			t.Errorf("state %q escaped the tenant domain: %q", state, loc)
		}
	}
}

// States that are not of the form this service issues must be refused, not
// partially interpreted.
func TestRefusesMalformedState(t *testing.T) {
	f := newTestForwarder(t, "")
	hostile := []string{
		"evil.com.nonce",
		"acme.evil.com.nonce",
		"//evil.com.nonce",
		"a/b.nonce",
		"a%2fb.nonce",
		"-evil.nonce",
		"ACME.nonce",
		"acme@evil.com.nonce",
		"nonce-with-no-dot",
		"",
	}
	for _, state := range hostile {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/oauth/callback?code=AUTHCODE", nil)
		q := req.URL.Query()
		q.Set("state", state)
		req.URL.RawQuery = q.Encode()
		f.HandleCallback(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("state %q: status %d, want 400 (Location %q)",
				state, rec.Code, rec.Header().Get("Location"))
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("state %q produced a redirect to %q", state, loc)
		}
	}
}

// The refusal page must not reflect attacker-controlled state back to the
// browser.
func TestRefusalDoesNotEchoState(t *testing.T) {
	f := newTestForwarder(t, "")
	rec := httptest.NewRecorder()
	f.HandleCallback(rec, httptest.NewRequest("GET",
		"/oauth/callback?state=%3Cscript%3Ealert(1)%3C/script%3E.n", nil))

	if body := rec.Body.String(); strings.Contains(body, "script") {
		t.Errorf("refusal echoed the state back: %q", body)
	}
}

func TestAllowlistRestrictsTenants(t *testing.T) {
	f := newTestForwarder(t, "acme, globex")

	rec := httptest.NewRecorder()
	f.HandleCallback(rec, httptest.NewRequest("GET", "/oauth/callback?state=acme.n", nil))
	if rec.Code != http.StatusFound {
		t.Errorf("allowlisted tenant: status %d, want 302", rec.Code)
	}

	rec = httptest.NewRecorder()
	f.HandleCallback(rec, httptest.NewRequest("GET", "/oauth/callback?state=wayne.n", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-allowlisted tenant: status %d, want 400", rec.Code)
	}
}

func TestNewRejectsUnsafeConfig(t *testing.T) {
	bad := []struct{ tmpl, path, allow, why string }{
		{"https://gui.example.com", DefaultTenantPath, "", "no {slug} placeholder"},
		{"http://{slug}.gui.example.com", DefaultTenantPath, "", "not https"},
		{"https://{slug}.gui.example.com/sub", DefaultTenantPath, "", "template carries a path"},
		{"https://{slug}.gui.example.com?x=1", DefaultTenantPath, "", "template carries a query"},
		{"https://{slug}.gui.example.com", "no-leading-slash", "", "tenant path is relative"},
		{"https://{slug}.gui.example.com", DefaultTenantPath, "evil.com", "invalid slug in allowlist"},
		{"https://{slug}.gui.example.com", DefaultTenantPath, " , ", "allowlist set but empty"},
	}
	for _, tc := range bad {
		if _, err := New(tc.tmpl, tc.path, tc.allow); err == nil {
			t.Errorf("New accepted %s (%q)", tc.why, tc.tmpl)
		}
	}
}
