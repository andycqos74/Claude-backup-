# Phase 1 prototype — two tenants by hand

The multi-tenant agent plane from [docs/multi-tenant-design.md](../../docs/multi-tenant-design.md),
with no control plane: tenants are added by editing files here.

| File | What |
|---|---|
| `docker-compose.yml` | Two tenant containers, the nginx SNI router, cloudflared |
| `nginx-sni.conf` | The router: SNI → tenant, without terminating TLS |

## The idea in one paragraph

Agents pin their tenant's certificate fingerprint, so nothing may terminate
TLS in front of them — which rules out the tunnel and any HTTP router. nginx
`ssl_preread` reads the hostname out of the ClientHello and splices the raw
TCP stream to the right tenant, so each agent still receives its own tenant's
certificate and the pin holds. The GUI has no such constraint and goes
through cloudflared as usual.

```
browser ─https─▶ Cloudflare ─tunnel─▶ cloudflared ─http──▶ <tenant>:8080
agents  ─https, pinned──────────────▶ :8443 nginx ssl_preread ─▶ <tenant>:8443
```

## Run it

```bash
CB_OWNER=<github-owner> CB_TAG=<image-tag> CB_TUNNEL_TOKEN=<token> \
  docker compose -f deploy/multi-tenant/docker-compose.yml up -d
```

Then per tenant: a public hostname in the Cloudflare dashboard
(`HTTP` → `<tenant>-server:8080`) and a DNS **A** record for the agent
hostname pointing at this host, grey-cloud / DNS-only.

## Prove it works

```bash
scripts/e2e-multi-tenant.sh
```

Starts two real servers with different certificates, a real nginx, enrolls a
real agent through the router and runs a real backup — then checks that
tenants cannot see each other, that replaying one tenant's credentials
against another's hostname fails the pin, and that a connection without SNI
is refused rather than misrouted.

Needs `go`, `jq`, `curl` and nginx built with `ngx_stream_ssl_preread_module`
(Debian/Ubuntu: `apt install nginx-light libnginx-mod-stream`). Uses
`*.localtest.me`, so no `/etc/hosts` edit and no root.

## Adding a tenant

1. A service block in `docker-compose.yml` (copy an existing one) and a
   volume with an explicit `name:`.
2. A line in the `map` in `nginx-sni.conf`, then `nginx -s reload`.
3. A public hostname in Cloudflare for the GUI.
4. A DNS A record for the agent hostname.

## Two rules that bite

**Agent hostnames must be DNS names, never bare IPs.** Go omits SNI for IP
literals, so an IP-enrolled agent arrives with no hostname and the router
refuses it. The `default ""` in the map makes that a clean refusal instead of
a silent misroute into another tenant.

**Never publish a tenant's 8443 directly.** It would bypass the router and,
worse, give that tenant a second address whose enrollment commands disagree
with `CB_PUBLIC_URL`.
