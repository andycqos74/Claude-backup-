# Target SaaS model — gap analysis

How far the current code is from the intended product, what each gap costs,
and what the alternatives are at each layer.

**Terminology.** This document says **tenant** for a paying customer (one
Docker stack, one subdomain) and **agent** or **endpoint** for a backed-up
machine (the thing the Windows installer installs). The repository and GUI
call the latter a "client", which collides with the business sense of the
word; both are kept apart here deliberately.

**Supersedes** the assumption in
[multi-tenant-design.md](multi-tenant-design.md) that some customers would
self-host. They will not. Everything runs in the operator's infrastructure,
which removes the "no Cloudflare dependency" constraint and makes several
otherwise-awkward options viable — see §6.

---

## 1. The target

1. Web GUI behind SSL (cloudflared or an alternative).
2. Multiple tenants, each on its own Docker stack.
3. Each tenant on a subdomain of one shared top-level domain. Custom
   domains are a later phase.
4. Endpoint registration as simple as possible. The current Windows `.exe`
   is acceptable; a self-contained installer that does not need a command
   prompt would be better. A Linux command line is fine. Android and iOS
   are a later phase.
5. Backup, restore and file transfer keep working exactly as they do now.

---

## 2. Scorecard

| # | Requirement | State | Remaining work |
|---|---|---|---|
| 1 | GUI behind SSL | ✅ **shipped** | Fan-out for many tenants (small) |
| 2 | Tenant per Docker stack | 🟡 **prototyped** | Provisioning control plane (medium) |
| 3 | Subdomain per tenant | 🟡 **prototyped** | Wildcard DNS + GUI fan-out (small) |
| 4 | Simple endpoint registration | 🔴 **the real gap** | Double-click mechanics (small) + code signing (medium, one spike first) |
| 5 | Backup / transfer unchanged | ✅ **proven** | None |

Three of the five are close. Requirement 4 is where the actual work is. It
turned out to be less entangled with the security model than it first
appeared — see §4.

---

## 3. Requirements 1–3, 5: close

### 1. GUI behind SSL — shipped

`CB_GUI_LISTEN` serves the GUI, admin API, `/static` and `/dl` over plain
HTTP for a TLS-terminating proxy; `/api/agent/*` is deliberately absent, so
a tunnel cannot expose enrollment or blob storage. `CB_GUI_URL` keeps the
browser-facing origin (and the OAuth redirect) separate from the agent
address. See [cloudflared.md](cloudflared.md).

**Routing many tenants through one tunnel works fine.** A single cloudflared
instance evaluates an ordered list of ingress rules and forwards each
hostname to a different origin, so one tunnel serves every tenant:

```yaml
ingress:
  - hostname: acme.gui.example.com
    service: http://acme-server:8080
  - hostname: globex.gui.example.com
    service: http://globex-server:8080
  - service: http_status:404          # required catch-all
```

The only thing a *wildcard* rule changes is where the per-tenant line lives.
`*.gui.example.com` matches every subdomain but forwards them all to one
origin — wildcards match broadly, they do not fan out — so a wildcard only
helps if something behind it fans out by `Host`.

Both shapes are viable and the choice is about where per-tenant config
lives, not about capability:

| Shape | Adding a tenant | Components |
|---|---|---|
| One tunnel, explicit rule per tenant | one ingress rule (config reload or dashboard/API call) | cloudflared only |
| Wildcard rule → internal proxy | nothing, if the proxy self-discovers | cloudflared + Traefik/nginx |

Explicit rules are the simpler default: the provisioner is already creating a
container, a volume and a DNS record, so adding an ingress rule is one more
idempotent step in a sequence it already owns. Reach for the wildcard shape
only if you want tenant stacks to be entirely self-describing.

### 2. Tenant per Docker stack — prototyped

`deploy/multi-tenant/` runs two tenants side by side today. What is missing
is everything that makes it a service rather than a demo: a tenant registry,
provisioning, and the routing config being generated rather than edited.
Design in [multi-tenant-design.md §6](multi-tenant-design.md); the
generate-don't-execute recommendation matters more now, because the control
plane is internet-facing and holding a Docker socket would make a bug in it
host root.

### 3. Subdomain per tenant — prototyped, with one caveat

The agent plane is proven: `scripts/e2e-multi-tenant.sh` routes
`acme.backup.example.com` and `globex.backup.example.com` to separate
containers by SNI, with pinning intact.

**Caveat, already load-bearing:** agents must be enrolled with a DNS
hostname. Go omits SNI for IP literals, so an IP-enrolled agent is
unroutable; the router's `default ""` refuses it rather than misrouting it.
The subdomain model makes this natural rather than a constraint.

**Gap:** wildcard DNS. A single `*.gui` and `*.backup` record removes the
per-tenant DNS step entirely. Confirm wildcard proxied records are available
on your Cloudflare plan before depending on it; the fallback is one record
per tenant, which provisioning creates anyway.

Custom domains later are a genuine step up in difficulty — per-domain
certificates, per-domain OAuth redirect registration, and SNI routing that
is no longer a single wildcard. Worth keeping out of phase 1 as planned.

### 5. Backup / transfer unchanged — proven

The SNI router preserves end-to-end TLS, so the agent protocol is untouched:
control channel, blob upload/download, manifests, restores and file
transfers all behave as today. The e2e test performs a real backup over the
routed connection and confirms tenant isolation. No work required.

One operational note: file **pushes** through the GUI are capped at 100 MB
by Cloudflare's proxied request-body limit. That affects the operator's
push-a-file feature, not backups, which never cross the tunnel.

---

## 4. Requirement 4: endpoint registration — where the work is

### What happens today

Download from the tenant's GUI → the server appends a JSON blob
(`server_url`, `token`, `fingerprint`, `name`) to the agent binary
(`internal/bundle`) → the customer runs
`backup-agent-installer.exe install` **from an elevated prompt** → it
enrolls and installs the service.

That is already better than a pasted command. Three things stop it being
double-click:

1. **No-args does nothing useful.** `main()` calls `usage()` and exits 2
   when `len(os.Args) < 2` (`cmd/agent/main.go:69`). A double-click flashes
   a console window and vanishes.
2. **No UAC manifest.** The binary has no embedded manifest requesting
   elevation, so a double-click runs unelevated and service installation
   fails with "run as Administrator".
3. **No feedback.** It is a console application, so success or failure is
   printed into a window that closes.

Fixing those three is small — a `.syso` manifest resource, treating no-args
as `install` when an enrollment blob is embedded, and a result dialog.
Call it a few hundred lines including build changes.

### Signing: the constraint is weaker than it looks

An unsigned executable downloaded from the internet gets SmartScreen's
"Windows protected your PC", and an unsigned elevation prompt says "Unknown
publisher". For a product whose pitch is easy onboarding, that is the whole
experience. So: can the per-download embedded token survive code signing?

**Mostly yes** — but only with the right technique, and two widely-repeated
beliefs about this are out of date. Both were checked rather than recalled,
because the answers drive a purchasing decision.

**1. Appending to a signed executable does not necessarily break the
signature.** Authenticode does not hash the area past the last section in
the way a naive reading suggests; data placed after the attribute
certificate table is skipped by both the hash computation and signature
parsing. This is an established technique — legitimate installers use it to
embed per-download, user-specific data into an already-signed binary, and
there are public implementations of it for NSIS.

Two cautions before relying on it:

- **The payload should live inside the certificate table's declared length**
  (padding it to cover the appended bytes), not simply be dumped after the
  end of the file. `bundle.Append` currently does the latter
  (`internal/bundle/bundle.go:44`), which is fine for an unsigned binary but
  is not the form that reliably verifies.
- **Microsoft has added heuristics** against abuse of the signature block
  ("PECertInvalidMarkers"), because this same trick has been used to smuggle
  data past signature checks. The technique works, but it is close enough to
  something defensive tooling watches for that it must be **tested against
  current Windows and current Defender** before the release process depends
  on it.

**2. EV certificates no longer grant instant SmartScreen reputation.**
Microsoft removed that behaviour in 2024; EV and OV now build reputation the
same way. This retires the usual "buy EV to skip the warning" advice, and
with it the hardware-token complexity in the release process that EV implied.
(Note that CA/Browser Forum rules now require private-key hardware protection
for OV code signing too, so some token handling is unavoidable either way.)

What actually governs the warning is that SmartScreen tracks **two** parallel
reputations: per-file-hash, and per-certificate-thumbprint. A unique
per-download binary has no file reputation and never will — but it inherits
the publisher reputation of the certificate it is signed with, and that
reputation carries across versions signed under the same identity. So:

> Per-download unique installers and code signing are compatible. Sign every
> generated binary with one consistent publisher identity, let the
> certificate accumulate reputation, and each unique download benefits from
> it.

### What that means for the options

| Option | How | Trade-off |
|---|---|---|
| **A. Keep embedding, add signing** | Sign the base agent, then append the enrollment blob inside the padded certificate table | **Zero typing preserved.** Needs the padding technique and ongoing verification against Windows/Defender behaviour |
| **B. Sign each generated binary server-side** | `osslsigncode` at download time | No append trickery at all. But the signing key lives on an internet-facing host — a serious concession |
| **C. Signed generic installer + enrollment code** | One signed binary per release; token typed in or read from a companion file | Simplest, most robust, fully cacheable download. Costs the user one short code |
| **D. Unsigned per-download installer** | Today | Zero work; SmartScreen warning forever |

**A is the recommendation, with C as the fallback** if the append technique
proves fragile in testing. A keeps today's genuinely good property — one
file, nothing to type — and adds signing on top. C is worth understanding
regardless, because it has a second benefit: the installer becomes a single
static file that can be cached or put on a CDN, rather than being generated
per download.

If C is ever adopted, the subdomain model makes the code compact:

```
    ACME-7F2K-9QX1
     │     └── one-time token
     └── tenant slug → https://acme.backup.example.com:8443
```

### An independent simplification: the certificate model

Separate from the installer question, the fingerprint is worth revisiting now
that every tenant sits on a subdomain of **one domain you control**.

A wildcard certificate from a public CA (`*.backup.example.com`, DNS-01,
renewed centrally, mounted into each stack via the existing
`CB_TLS_CERT`/`CB_TLS_KEY` — no code change needed to deploy it) makes
ordinary CA validation possible and removes the fingerprint from the
enrollment payload entirely.

| | Pinned self-signed (today) | Public wildcard certificate |
|---|---|---|
| Trust anchor | The exact certificate | The CA, plus DNS control |
| Enrollment payload | Carries a 64-char fingerprint | Token + tenant slug only |
| Certificate renewal | Breaks every agent | Transparent |
| Tenant separation at TLS | Each tenant a distinct certificate | All tenants share one certificate |
| Exposure | Immune to a mis-issued public certificate | A mis-issued certificate for your domain is a real risk |
| Operational cost | None | Central renewal must never fail for 90 days |

Note the fourth row: with a shared wildcard, an agent pointed at the wrong
tenant's subdomain completes TLS and is then rejected by agent-id/secret
authentication at the application layer, rather than during the handshake.
Isolation still holds; it moves one layer up.

**Middle path:** public certificate for validation *plus* SPKI
(public-key) pinning instead of leaf-certificate pinning. The key survives
renewal, so agents keep a pin that does not break every 90 days.

This is no longer forced by the installer design — option A keeps the
fingerprint embedded painlessly — so treat it as an independent question,
decided on renewal ergonomics rather than on enrollment payload size.

### MSI and mass deployment

MSPs push software through an RMM, which runs a command line — so a signed
`.exe` with flags covers that case, and an MSI is not required. An MSI earns
its place only for GPO/Intune deployment, and per-tenant data would go in as
properties (`msiexec /i cb.msi CODE=ACME-7F2K-9QX1`) rather than embedded.
Defer it.

### Mobile, when it comes

Flagging early because it affects how the endpoint protocol is framed: iOS
cannot run a continuously-resident backup agent. There is no equivalent of a
Windows service, background execution is heavily constrained, and an app can
only reach its own sandbox plus what the user grants through Files/Photos.
Android is closer but still fights the battery optimiser. Mobile will not be
the same agent with a different build target; it will be an opportunistic
sync of a user-selected scope. Worth not painting the protocol into a corner
that assumes a persistent control channel.

### Per-tenant storage backends — supported, with two constraints

Storage configuration is per container (provider, folder, OAuth client
credentials and refresh token all live in that tenant's own `settings`
table), so "each tenant connects their own OneDrive / S3 / local folder"
needs no work. Two things do need decisions.

**1. Who owns the OAuth app registration?** Today the admin pastes their own
client ID and secret (`internal/server/storage/config.go`), so each tenant
brings their own Azure or Google application. That has no scaling ceiling,
but it is a poor fit for "registration as simple as possible" — asking a
customer to create an Azure app registration is the least simple step in the
whole product.

The alternative, an operator-owned application that every tenant connects to
with one click, hits a hard limit: **Microsoft allows at most 100 redirect
URIs for apps supporting personal accounts and 250 for work accounts, and
the limit cannot be raised.** Since each tenant's callback lives on its own
subdomain, that is a ceiling of 100 tenants on consumer OneDrive.

Microsoft's own recommendation for this case is the `state` parameter: one
registered redirect URI on a central hostname, with `state` carrying which
tenant to return to. That fits here with a small, stateless addition —
register `https://connect.example.com/oauth/callback` once, and have it
validate the tenant slug out of `state` against the tenant registry and
302 the browser on to that tenant's own callback with `code` and `state`
intact. The tenant container then performs the token exchange exactly as it
does now, unchanged. The slug **must** be validated against the registry, or
the forwarder is an open redirect carrying an authorization code.

**2. Turning off local storage needs a code change.** `initStorage` treats
local disk as both the default and the fallback: if a configured cloud
backend fails to open, it logs and silently switches to local
(`internal/server/storage.go`). In a hosted model that is the wrong
behaviour — a tenant whose OneDrive token is revoked would quietly start
filling the container's volume instead of failing loudly. Disabling local
storage properly means a mode where an unavailable backend is a hard error:
refuse to accept blobs, mark the tenant degraded, and surface it, rather than
falling back.

---

## 5. Pros and cons of the model as specified

### Pros

- **Isolation is a container boundary.** No `WHERE tenant_id = ?` to forget.
  A cross-tenant data leak requires a routing failure, not a missing filter.
- **No core rewrite.** The server stays single-tenant, which is what keeps
  it small enough to reason about.
- **Blast radius and upgrades are per tenant.** Canary one tenant; a bad
  migration cannot take out the fleet.
- **Per-tenant storage falls out free.** Tenant A on B2, tenant B on their
  own OneDrive, already supported.
- **The agent protocol is untouched**, so requirement 5 costs nothing.
- **Subdomains make SNI routing natural**, and make the enrollment code idea
  possible.

### Cons

- **No cross-tenant deduplication.** Deliberate: a shared content-addressed
  store would make `/api/agent/blobs/check` an existence oracle across
  tenants. But it does mean fifty tenants with the same Windows files store
  fifty copies. **This is the main cost-of-goods consequence of the model**
  and should be modelled against storage pricing before committing.
- **Per-tenant idle cost.** A Go process plus SQLite per tenant, always
  running even for a tenant with three endpoints. Fine at tens, worth
  measuring at hundreds.
- **N containers to upgrade**, each with its own certificate and its own
  chance to fail. Needs rolling-update tooling early, not late.
- **Fleet-wide questions are awkward.** "How much storage across all
  tenants?" means querying N SQLite databases. A reporting rollup becomes
  its own small job.
- **Cloudflare sees admin sessions** in cleartext if the tunnel is used.
  Since there are no self-hosted installs, this is now purely your own
  disclosure decision rather than a customer constraint — but it is still
  worth saying in a DPA.
- **Custom domains later will be disproportionately harder** than the
  subdomain phase: per-domain certificates and per-domain OAuth redirects.

---

## 6. Other viable options

### For the GUI plane

| Option | Adding a tenant needs | Inbound port | Notes |
|---|---|---|---|
| **cloudflared, one rule per tenant** | a dashboard entry | none | Where the prototype is |
| **cloudflared wildcard → internal proxy** | nothing | none | Wildcard ingress `*.gui.example.com` to a Traefik/nginx that fans out by Host |
| **Traefik + Let's Encrypt (DNS-01)** | nothing | 443 | Each stack declares its hostname with Docker labels; Traefik picks it up automatically |
| **Caddy + DNS-01 wildcard** | a config line | 443 | Simplest to reason about; no third party |

**cloudflared alone is enough**, and is the recommended starting point: one
tunnel, one ingress rule per tenant, added by the provisioner alongside the
container and DNS record it is already creating.

Traefik earns its place only if you want tenant stacks to be entirely
self-describing — a new stack carries its own routing labels and needs no
central change at all. That is a real benefit at high tenant churn, and the
cost is a second component plus Traefik's access to the Docker socket.
It is an optimisation to reach for later, not a starting requirement.

### For the agent plane

| Option | Trade-off |
|---|---|
| **SNI router, self-signed per tenant** (proven) | No new dependencies, pinning intact, fingerprint must be transported |
| **Wildcard public certificate, CA validation** | Enrollment code becomes short; renewal transparent; shared certificate across tenants |
| **Wildcard public certificate + SPKI pinning** | Keeps a pin that survives renewal; best of both; needs a change to `pinnedTLSConfig` |
| **Port per tenant** | No router; ugly beyond ~10 tenants; leaks tenant count |

### For registration

| Option | Experience |
|---|---|
| **Signed per-download installer** (recommended) | One file, zero typing, no warning once the certificate has reputation. Depends on the padded-append spike in §4 |
| **Signed generic installer + typed code** | Download once, double-click, type `ACME-7F2K-9QX1`. Most robust; cacheable download; costs one short code |
| **Signed generic installer + `.cbenroll` file** | Two files in Downloads; no typing; slightly more to explain |
| **Unsigned per-download installer** (today) | One file, zero typing, SmartScreen warning every time |
| **MSI with properties** | RMM/Intune only; not a double-click story |

---

## 7. Recommendation

**Target:** a signed per-download installer that keeps today's zero-typing
flow, cloudflared at the edge with Access, an internal proxy fanning out to
tenant stacks, and wildcard DNS so adding a tenant touches nothing external.
The certificate model is a separate decision, not a prerequisite.

Ordered by ratio of value to risk:

1. **GUI fan-out** — wildcard ingress into an internal proxy. Small, removes
   the per-tenant dashboard step. *No security implications.*
2. **Double-click installer mechanics** — UAC manifest, no-args install,
   result dialog. Small, independently useful, and worth doing *before* the
   trust-model decision because it is orthogonal to it.
3. **Code signing, with a spike first.** Before buying anything, test
   whether a signed binary survives the padded-certificate-table append on
   current Windows and Defender. That spike decides between option A
   (keep zero typing) and option C (enrollment code), and it is a day's work
   against a trial certificate. EV is no longer worth paying for.
4. **Certificate model — independent.** Wildcard public certificate with
   SPKI pinning removes the 90-day renewal cliff and simplifies enrollment,
   but nothing above depends on it. Prototype against the existing e2e test
   and decide on its own merits.
5. **Provisioning control plane** — generate-don't-execute, per
   [multi-tenant-design.md §6](multi-tenant-design.md).
6. **Fleet tooling** — rolling upgrades and a storage-rollup report. Boring,
   and the thing that hurts at thirty tenants if skipped.

Steps 1 and 2 can start now. Step 3 should be prototyped and measured before
it is committed to, because it is the only item here that changes the
security model.

---

## 8. Open questions

- **Storage economics without cross-tenant dedup.** Worth modelling before
  committing: what does fifty tenants' worth of near-identical Windows files
  cost at your storage prices?
- **Does the padded-append technique hold** on current Windows and Defender?
  This is the one experiment that changes the installer design, and it is
  cheap to run. Everything else in §4 follows from the answer.
- **Does `*.backup.example.com` cover the agent plane too**, or do agents get
  their own domain? One certificate is simpler; two make it easier to move
  the GUI behind Cloudflare while agents resolve elsewhere.
- **What is the enrollment code's lifetime and scope?** One per endpoint, or
  one reusable per tenant with a device limit? The latter is much simpler for
  an admin rolling out fifty machines, and weaker if leaked.
