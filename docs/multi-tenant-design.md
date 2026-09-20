# Multi-tenant design

Status: **proposal**. Nothing here is implemented. It covers four questions:

1. What shape should multi-tenancy take?
2. How do the GUI and sign-on get SSL — is `cloudflared` the right tool?
3. How does a central admin create tenants?
4. Is the job/orchestration layer a fit for Temporal?

The short version: **one single-tenant server container per tenant**, fronted
by a small control plane that handles routing, sign-on and provisioning. Do
not make the backup server itself tenant-aware. Do not adopt Temporal for
backup runs; it is a good fit for *provisioning* if that grows, and a poor
fit for the data plane.

**Who the tenants are** (answered, then revised): managed-service customers
only — **there will be no customer self-hosted installs**. Everything runs in
the operator's infrastructure.

That removes the "no Cloudflare dependency" constraint recorded in an earlier
revision of this document, and with it the requirement that trusted-IdP
support be generic OIDC before it is useful. Depending on Cloudflare Access
is now a straightforward operator choice.

It does *not* change the Model B decision in §2, which rests on isolation and
on not rewriting the server — both of which stand on their own.

The concrete target model, and how far the code is from it, is in
[saas-model-gap-analysis.md](saas-model-gap-analysis.md).

---

## 1. Where the code stands today

Facts that constrain every option below.

**There is no tenant concept.** The schema (`internal/server/store/store.go`)
is `users, sessions, enroll_tokens, settings, agents, jobs, runs, run_logs,
snapshots, blobs`. No `tenant_id` column anywhere, one admin user set, one
storage backend config in `settings`, one TLS certificate, one websocket hub.

**The GUI is not separable from its backend.** Page handlers check
`store.CountUsers()` and `sessionUser(r)` against the container's own SQLite
(`internal/server/web.go:52`), and every template hardcodes root-absolute API
paths (`/api/admin/jobs`, `/api/admin/agents`, …), with `app.js` redirecting
to `/login` on a 401. A central GUI calling per-tenant APIs is a rewrite, not
a configuration.

**Consequence: path-prefix routing does not work.**

| Route | Works | Why |
|---|---|---|
| `acme.gui.example.com` → acme container | ✅ | Origin-relative paths resolve within the tenant |
| `gui.example.com/t/acme/` → acme container | ❌ | The browser requests `/api/admin/jobs` at the root |

**Agents pin the server certificate** (`internal/agent/creds.go:56`), so
nothing may terminate TLS in front of them. That rules out the tunnel *and*
any HTTP-level router for agent traffic — see §4.

**Orchestration is small.** `sched.go` (66 lines), `hub.go` (139),
`jobs.go` (235). Durable run state already lives in SQLite. This matters for
the Temporal question in §7.

---

## 2. Two models considered

### Model A — one server, `tenant_id` everywhere

Add a tenant column to every table, scope every query, partition the hub and
the storage keyspace.

- ✅ One container, one port, one certificate, one upgrade.
- ✅ Cross-tenant deduplication.
- ❌ Touches every query, the scheduler, GC, retention and storage layout.
- ❌ **Dedup becomes a data-leak oracle.** Storage is content-addressed and
  `POST /api/agent/blobs/check` reports whether a hash already exists. Share
  one blob store and any tenant can test "does another tenant hold this exact
  file?" Partitioning the keyspace to avoid it discards the dedup benefit,
  which was the main reason to share.
- ❌ One bug or one bad migration affects every tenant at once.

### Model B — one single-tenant container per tenant  ✅ recommended

Each tenant gets a `backup-server` container, its own volume, SQLite,
certificate/fingerprint, storage backend and OneDrive connection.

- ✅ **No changes to the backup server's core.** Its single-tenant simplicity
  is its main virtue; this preserves it.
- ✅ Isolation is a container boundary, not a `WHERE` clause. A missing scope
  filter cannot leak across tenants because there is nothing to leak into.
- ✅ Per-tenant storage backend falls out for free — tenant A on B2, tenant B
  on their own OneDrive.
- ✅ Blast radius and upgrades are per tenant; canary a tenant before the rest.
- ❌ No cross-tenant dedup (see above — this is a feature).
- ❌ N containers to operate, N certificates, N upgrades.
- ❌ Per-tenant idle cost: a Go process plus SQLite. Small, but not zero.

**Decision: Model B.** The isolation argument and the "no core rewrite"
argument both point the same way, and the dedup loss is a security win.

---

## 3. Architecture

```
                        ┌──────────────────────────────┐
  operator's browser    │  Cloudflare edge             │
      │                 │   • TLS certificate          │
      └────https────────▶   • Access (identity)        │
                        └──────────────┬───────────────┘
                                       │ tunnel (outbound only)
                                ┌──────▼──────┐
                                │ cloudflared │
                                └──────┬──────┘
                       acme.gui…       │       globex.gui…
                          ┌────────────┴────────────┐
                          │                         │
                   ┌──────▼──────┐           ┌──────▼──────┐
                   │ acme-server │           │globex-server│
                   │  :8080 GUI  │           │  :8080 GUI  │
                   │  :8443 agts │           │  :8443 agts │
                   └──────▲──────┘           └──────▲──────┘
                          │                         │
                          └──────────┬──────────────┘
                                     │  proxy_pass, no TLS termination
                          ┌──────────┴──────────┐
                          │ nginx stream :8443  │
                          │ ssl_preread on      │
                          └──────────▲──────────┘
                                     │ https, certificate pinned
                                  agents
```

Two planes, deliberately separate:

- **GUI plane** — browser traffic, terminated by Cloudflare, routed by
  hostname to each tenant's `CB_GUI_LISTEN`. This is the mechanism already
  shipped in [cloudflared.md](cloudflared.md), fanned out to N backends.
- **Agent plane** — never terminated. One TCP port, routed by SNI.

---

## 4. The agent plane: why it needs its own answer

Agents verify the server by the SHA-256 fingerprint of the certificate they
are handed. Any proxy that terminates TLS presents its own certificate and
the pin fails — by design. So agents cannot go through the tunnel, and an
HTTP router cannot fan them out by hostname either.

### Option 1 — port per tenant

`8443` → acme, `8444` → globex, … Works with today's code;
`CB_PUBLIC_URL` already carries a port into enrollment commands and
installers.

Cost: firewall and NAT churn per tenant, and port numbers leak tenant count.
Fine to ~10 tenants.

### Option 2 — SNI passthrough on one port  ✅ preferred

`nginx` `stream` with `ssl_preread` routes on the hostname in the ClientHello
without decrypting anything, so the pin survives end to end:

```nginx
stream {
  map $ssl_preread_server_name $tenant {
    acme.backup.example.com    acme-server:8443;
    globex.backup.example.com  globex-server:8443;
    default                    "";           # refuse unknown names
  }
  server {
    listen 8443;
    ssl_preread on;
    proxy_pass $tenant;
  }
}
```

**Verified, not assumed.** The agent's `pinnedTLSConfig` sets
`InsecureSkipVerify` with a custom verifier and no explicit `ServerName`, so
whether SNI is present at all is not obvious. Probing a TLS server that
captures the ClientHello, using the real config:

```
dialed by localhost -> SNI the server saw: [localhost]
dialed by 127.0.0.1 -> SNI the server saw: []
```

Go populates SNI from the URL host, and omits it for IP literals (per RFC
6066). Therefore:

> **Hard rule: tenants must be enrolled with a DNS hostname.** An agent whose
> `CB_PUBLIC_URL` is a bare IP sends no SNI and cannot be routed. With
> `default ""` above it is refused rather than silently landing in the wrong
> tenant — fail closed, and make the provisioner reject IP addresses.

Each tenant keeps its own certificate and fingerprint; the router only picks a
backend.

---

## 5. SSL and sign-on for the GUI

### Is `cloudflared` the right tool?

For this system, yes — with one caveat. Comparison of the realistic options:

| Approach | Inbound port | Certificate | Identity in front of login | Dependency |
|---|---|---|---|---|
| **Cloudflare Tunnel + Access** | none | Cloudflare, automatic | ✅ built in, free tier | Cloudflare account + domain |
| Caddy + Let's Encrypt (DNS-01) | 443 | automatic, renews | ❌ needs a separate OIDC proxy | none |
| Traefik + Let's Encrypt | 443 | automatic | ❌ ForwardAuth + separate IdP | none |
| Tailscale Funnel / `tsnet` | none | automatic | ✅ tailnet ACLs | Tailscale account |
| nginx + certbot | 443 | manual-ish | ❌ | none |

**Recommendation: Cloudflare Tunnel + Access**, because it is the only option
that gives all three of *no inbound port*, *automatic certificates* and
*identity in front of the login page* without assembling an IdP yourself.
Wildcard routing (`*.gui.example.com`) means adding a tenant needs no new
certificate and no new port.

**The caveat, stated plainly:** it puts Cloudflare in the path of every
admin session for every tenant, and they can see that traffic in cleartext.
For a backup product, some customers will have an opinion about that. It
never touches backup *data* — that flows agent → server on the pinned 8443
path, which Cloudflare never sees — but it does cover restore downloads
initiated from the GUI. If that is unacceptable, **Caddy with a DNS-01
wildcard certificate plus an OIDC proxy** is the self-hosted equivalent, at
the cost of an open 443 and running your own identity provider.

Tailscale Funnel is a genuine third option if every operator is staff rather
than customer — identity and transport in one, no public surface at all —
but it does not suit customer-facing tenant admins.

### Single sign-on across tenants

Today each tenant container has its own `users` table, so an operator with
three tenants logs in three times. The tempting fix — a central GUI that
authenticates and calls tenant APIs — is the rewrite ruled out in §1.

The cheap fix instead: **have tenant containers trust the identity the edge
already proved.** Cloudflare Access signs every proxied request with a
`Cf-Access-Jwt-Assertion` header. A new, off-by-default mode:

```
CB_TRUSTED_IDP_JWKS=https://<team>.cloudflareaccess.com/cdn-cgi/access/certs
CB_TRUSTED_IDP_AUD=<application AUD tag from the Access dashboard>
CB_TRUSTED_IDP_ISS=https://<team>.cloudflareaccess.com
```

When set, the server verifies the JWT's signature, `aud`, `iss` and `exp`,
takes `email` as the identity, and issues its own session cookie for the
matching (or auto-provisioned) user. Login once at the edge, land in any
tenant you are entitled to.

Non-negotiables for that feature:

- **Only trust the header on the GUI listener.** On `CB_LISTEN` it must be
  ignored outright — anyone who can reach 8443 could otherwise forge it.
  The listener split already shipped makes this a one-line distinction.
- **Verify the signature.** Never trust `Cf-Access-Authenticated-User-Email`,
  which is an unsigned convenience header.
- **Verify `aud`.** Without it a token minted for *any* application in the
  same Access team is accepted.
- **Keep password login working** as the break-glass path for when the edge
  is misconfigured, reachable only from the direct address.

Roughly 150 lines plus tests, against a GUI-splitting rewrite of several
thousand.

---

## 6. Tenant provisioning by a central admin

### The control plane

A small, separate program — **not** part of the backup server:

```
tenants (id, slug, display_name, created_at, state,
         gui_hostname, agent_hostname, agent_fingerprint,
         storage_backend, container_id, volume_name)
entitlements (email, tenant_id, role)
```

It owns: the tenant registry, the provisioning lifecycle, rendering the nginx
SNI map and the tunnel ingress config, and answering "which tenants may this
email see?" for the portal page. It never touches backup data.

### Provisioning lifecycle

```
create → allocate slug + hostnames → render compose → start container
       → wait healthy → seed admin → capture fingerprint → publish routes
       → active
```

Each step must be idempotent and each failure must roll back the ones before
it, because a half-created tenant that owns a hostname and a volume but has
no database is worse than no tenant.

Specific hazards from the current code:

- **Volume naming.** `deploy/portainer-stack.yml` already warns that a
  renamed stack silently creates *empty* volumes — a new certificate, a new
  fingerprint, and every enrolled agent breaks. The provisioner must name
  volumes explicitly per tenant (`cb-<slug>-data`) and refuse to start if the
  volume exists but the database does not.
- **There is no health endpoint.** `grep` finds no `/healthz`. "Wait healthy"
  currently means polling `GET /login` for a 200/302, which is a poor
  readiness signal. Add a real `/healthz` on the GUI listener that checks the
  database and the storage backend — small, and useful beyond provisioning.
- **Capturing the fingerprint** means reading it from the container log or
  the settings API. Better: a startup flag that writes it to a file, or a
  `/healthz` payload that includes it. Scraping logs in a provisioner is how
  you end up with a tenant whose enrollment commands are silently wrong.
- **Seeding the admin.** `POST /api/admin/setup` works exactly once and only
  while no user exists — usable, but it means the provisioner briefly holds
  the tenant's initial credentials. Prefer generating a one-time setup link
  for the customer over storing a password.

### The Docker socket problem

A control plane that starts containers needs the Docker socket, which is
**equivalent to root on the host** — the same caveat `docs/security.md`
already makes about the agent. Since this program is reachable from the
internet through the tunnel, that is a meaningful escalation: a bug in the
tenant-creation endpoint becomes host root.

Options, best first:

1. **Generate, don't execute.** The control plane writes a compose file and
   a systemd unit into a directory; a host-side `systemd.path` unit or a
   timer applies it. The web-facing process never holds the socket.
2. **A narrow provisioner sidecar** with no inbound network, exposing only
   `create/start/stop/destroy(slug)` over a unix socket, validating the slug
   against `^[a-z][a-z0-9-]{2,30}$`. It holds the Docker socket; the
   web-facing process does not.
3. **Docker socket directly in the web-facing container.** Simplest, and the
   one to avoid.

Do not let a tenant slug reach a shell, a path or a container name
unvalidated.

---

## 7. Does the rewrite simplify anything? What I would actually change

The instinct to "rewrite for multi-tenancy" should be resisted. The backup
server being single-tenant is what keeps it comprehensible, and Model B needs
none of it. The changes worth making are all small and independently useful:

| Change | Size | Why |
|---|---|---|
| `/healthz` on the GUI listener (db + backend + fingerprint) | small | Provisioning readiness; useful anyway |
| Trusted-IdP header auth, GUI listener only | ~150 lines | SSO across tenants without splitting the GUI |
| Persist the scheduler's last-fire per job | ~20 lines | Closes a real gap — see below |
| Sweep stale `running` runs at startup | ~10 lines | Closes a real gap — see below |
| Reject IP addresses in `CB_PUBLIC_URL` when SNI routing | tiny | Fails closed instead of mis-routing |

**Two durability gaps**, both pre-existing and both worth fixing regardless
of tenancy:

1. `runScheduler` keeps its `last` cursor **in memory**, initialised to
   `time.Now()` at startup (`sched.go:18`). A job due while the *server* was
   down is not run and not caught up — catch-up only covers the *agent* being
   offline. Restart the server at 02:59 and the 03:00 backups are silently
   skipped. Already tracked as [roadmap](roadmap.md) item 4.
2. Runs are marked failed when the *agent* disconnects
   (`api_agent.go:92` → `FailRunningRuns`), but nothing reconciles at server
   startup. A run that was `running` when the server restarted can stay
   `running` indefinitely. Not currently tracked.

These are the failure modes people reach for Temporal to solve. Here they are
about thirty lines.

---

## 8. Is this a fit for Temporal?

### What the orchestration actually is

```
scheduler tick (30s) → is the job due?
                     → agent online?  → INSERT run(status=running)
                                      → send one websocket message
                                      → …the agent does everything…
                                      → agent reports RunDone → UPDATE run
                     → agent offline? → mark catch-up pending
```

The server does not execute backups. It decides *when*, sends one message,
and records the outcome. The work — walking the filesystem, hashing,
compressing, uploading, verifying — happens inside the agent, on the
customer's machine.

### Why Temporal is a poor fit for backup runs

**The worker cannot join the cluster.** Temporal's model wants the thing
doing the work to be a worker polling a task queue. Here that is the agent,
on a customer machine. Making agents Temporal workers would mean shipping
Temporal client credentials to every customer endpoint and letting them poll
your cluster — against a security model whose entire premise is that an agent
holds nothing but its own scoped secret and a pinned fingerprint. Per-tenant
namespaces narrow the blast radius but do not change the shape: a compromised
laptop would hold a credential to your orchestration cluster.

**So Temporal could only wrap the server side** — which is one message send
and a callback. Wrapping that in a workflow buys durable retry of *sending a
message to a machine that is usually offline*, which the existing queue-plus-
catch-up already handles, and which Temporal cannot improve because the
constraint is the agent's availability, not the orchestrator's memory.

**The operational cost is real, though weaker than first argued.** A Temporal
deployment is a server, a database (Cassandra/MySQL/Postgres), and a UI — or
Temporal Cloud and a per-action bill.

An earlier revision of this document leaned on "the product is one container,
one port, a single static binary, and a cluster changes what it *is*". With
no customer self-hosted installs, that argument mostly falls away: the
cluster would be operator infrastructure that no customer ever sees, and
operators absorb dependencies that products cannot. What survives is the
ordinary version — it is a substantial new system to run, monitor and upgrade
for a fleet that a cron loop and a SQLite table are currently serving
adequately.

The verdict does not change, because it never rested on this argument. The
two that carry it are that the worker cannot join the cluster, and that the
server-side workflow is a single message send.

**And the durability gaps it would fix are thirty lines** (§7).

### Where Temporal genuinely would fit

**Tenant provisioning** (§6) is a textbook Temporal workflow: a long,
multi-step, partially-failing sequence across several systems — allocate,
render, start, wait healthy, seed, capture fingerprint, publish DNS and
routes — where every step needs retry and the whole thing needs compensating
rollback. If tenant counts reach the point where provisioning failures are
routine, this is where a workflow engine earns its keep, and it runs entirely
in your infrastructure with no agent involvement.

**Fleet-wide maintenance** is a weaker second: "for all tenants, prune, GC,
then verify a sample restore" as a scheduled fan-out with per-tenant retry.
Real, but a cron job and a work queue also do it.

### Verdict

> **Do not adopt Temporal for backup jobs.** The worker is an agent that
> cannot safely join the cluster, the server-side workflow is a single
> message send, and the durability gaps are cheaper to fix directly.
>
> **Reconsider it for the provisioning control plane** if tenant lifecycle
> management becomes a source of real operational pain. It is a genuinely
> good fit for that, and it is an isolated decision — the control plane is a
> new program, so adopting Temporal there commits nothing in the backup
> server.

If it is adopted later: namespace per tenant, Temporal Schedules replacing
the cron loop for control-plane work only, and the agent protocol untouched.

---

## 9. Phased plan

**Phase 1 — two tenants by hand. ✅ prototyped.** No control plane.
`deploy/multi-tenant/` holds a two-tenant compose stack and the nginx SNI
router config; `scripts/e2e-multi-tenant.sh` proves the mechanism end to end
against real binaries (see §11).

**Phase 2 — the small server changes.** `/healthz`, trusted-IdP auth, the two
durability fixes, IP rejection under SNI routing. Each is independently
useful and independently revertible.

**Phase 3 — the control plane.** Tenant registry, entitlements, a portal page
listing a user's tenants, and generate-don't-execute provisioning. Plain Go,
no workflow engine.

**Phase 4 — revisit.** Only if provisioning pain is real: Temporal for the
control plane.

---

## 10. Phase 1 prototype (implemented)

`deploy/multi-tenant/docker-compose.yml` — two tenant containers, an nginx
`agent-router` publishing the only port, and cloudflared for the GUI plane.
`deploy/multi-tenant/nginx-sni.conf` — the router, with a fail-closed map.

`scripts/e2e-multi-tenant.sh` runs the whole thing against real binaries: two
`backup-server` processes with genuinely different certificates, a real nginx
with `ngx_stream_ssl_preread_module`, a real `backup-agent`, and a real
backup. It uses `*.localtest.me` (public DNS pointing at 127.0.0.1), so it
needs no `/etc/hosts` edit and no root.

```
=== 1. The router hands each tenant its OWN certificate
  ok: SNI routing selects the right backend, certificate unmodified
=== 2. Enrolling an agent for acme, THROUGH the router
  ok: enrollment command points at the router, not the backend
  ok: agent enrolled through the router with the pin enforced
=== 3. A real backup over the routed connection
  ok: agent online in acme: fe3821444afe0cd9adc1
  ok: backup completed over the SNI-routed connection
=== 4. Tenant isolation
  ok: globex sees no agents and no snapshots of acme's
=== 5. The router cannot be used to cross tenants
  ok: crossing tenants fails the pin: fingerprint mismatch: got 15c5c9ff…
=== 6. No SNI (IP literal) is refused, not misrouted
  ok: connection without SNI refused by the router
```

What this establishes:

- **Pinning survives the hop.** `ssl_preread` selects a backend from the
  ClientHello and splices the TCP stream; the certificate the agent verifies
  is its own tenant's, byte for byte. This was the one assumption the whole
  model rests on.
- **The router is not a tenant-crossing tool.** Replaying tenant A's
  credentials against tenant B's hostname — same router, same port — fails
  the pin, because the pin is per tenant.
- **Fail-closed works.** A connection with no SNI (what an IP-enrolled agent
  sends) is refused rather than routed to a default backend.
- **Enrollment addresses stay correct.** Tokens minted through the GUI carry
  `https://<tenant-host>:<router-port>`, so installers point at the router.

Not covered by the prototype, and still to be proven: the Cloudflare tunnel
and Access policy (needs a real domain), per-tenant OAuth storage redirects,
and `nginx -s reload` under live agent connections when a tenant is added.

## 11. Open questions

- ~~**Who are the tenants?**~~ **Answered:** managed-service customers only;
  no self-hosted installs. One follow-on remains: Cloudflare terminates TLS
  for tenant admin sessions, which is a disclosure obligation (privacy
  policy / DPA). Backup data never crosses it; GUI-initiated restore
  downloads do.
- **Does a tenant admin ever need two tenants at once?** If not, SSO is a
  convenience; if yes, the portal page in Phase 3 becomes the main UI.
- **Per-tenant storage backends** — is each tenant expected to bring their own
  OneDrive/S3, or does the operator provide storage? Model B supports either;
  it changes who registers the OAuth application.
- **What is the upgrade contract?** All tenants on one version, or per-tenant
  pinning? Per-tenant pinning is possible with Model B and costs discipline.
