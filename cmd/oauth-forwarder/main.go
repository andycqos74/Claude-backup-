// oauth-forwarder is the central storage-OAuth callback for a multi-tenant
// deployment. It is the single redirect URI registered with each cloud
// provider, and its only job is to send the browser on to the tenant that
// started the flow.
//
// Providers cap registered redirect URIs per application (Microsoft: 100 for
// apps allowing personal accounts, 250 for work accounts, not raisable), so
// a callback per tenant subdomain puts a hard ceiling on tenant count. The
// documented alternative is one URI plus the OAuth state parameter, which is
// what this implements.
//
//	provider ──▶ https://connect.example.com/oauth/callback?code=…&state=acme.<nonce>
//	                            │ slug "acme", validated against the registry
//	                            ▼
//	             https://acme.gui.example.com/api/admin/storage/oauth/callback?code=…&state=…
//
// The tenant server then exchanges the code itself, exactly as it does in a
// single-tenant deployment. Nothing secret passes through here: no client
// secret, no token. The authorization code does, in a query string, so this
// process never logs the query.
//
// Configuration (environment):
//
//	CB_LISTEN           listen address (default :8080). Plain HTTP: put it
//	                    behind the same TLS-terminating proxy as the GUIs.
//	CB_TENANT_TEMPLATE  where a tenant lives, with {slug} substituted, e.g.
//	                    https://{slug}.gui.example.com          (required)
//	CB_CALLBACK_PATH    path this listens on (default /oauth/callback)
//	CB_TENANT_PATH      path on the tenant (default the server's own
//	                    /api/admin/storage/oauth/callback)
//	CB_TENANT_ALLOWLIST optional comma-separated slugs. When set, only these
//	                    are forwarded; otherwise any syntactically valid slug
//	                    is, which stays inside the template's domain.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"centralbackup/internal/oauthfwd"
)

func main() {
	log.SetFlags(log.LstdFlags)

	tmpl := strings.TrimSpace(os.Getenv("CB_TENANT_TEMPLATE"))
	if tmpl == "" {
		log.Fatal("CB_TENANT_TEMPLATE is required, e.g. https://{slug}.gui.example.com")
	}
	tenantPath := env("CB_TENANT_PATH", oauthfwd.DefaultTenantPath)
	f, err := oauthfwd.New(tmpl, tenantPath, os.Getenv("CB_TENANT_ALLOWLIST"))
	if err != nil {
		log.Fatalf("startup: %v", err)
	}

	callbackPath := env("CB_CALLBACK_PATH", "/oauth/callback")
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+callbackPath, f.HandleCallback)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok":true}`)
	})

	addr := env("CB_LISTEN", ":8080")
	log.Printf("oauth forwarder listening on %s, callback %s -> %s%s",
		addr, callbackPath, tmpl, tenantPath)
	if n, enforced := f.Tenants(); enforced {
		log.Printf("allowlist: %d tenant(s)", n)
	} else {
		log.Printf("allowlist: none (any valid slug within %s)", tmpl)
	}
	log.Fatal(http.ListenAndServe(addr, mux))
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
