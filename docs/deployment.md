# Deployment runbook

This walks through standing up the central server and enrolling your first
clients. There are two ways to run the server — pick one.

---

## Quick path: Docker server + Windows client

The condensed version for the most common setup. Full detail is in the
sections below.

**1. Server (Docker host with internet + Docker Compose):**

```bash
git clone <this-repo> && cd <repo>
CB_SERVER_NAME=<docker-host-ip-or-dns> \
  docker compose -f deploy/docker-compose.yml up -d --build
```

Publish/allow inbound **TCP 8443** to the Docker host. `CB_SERVER_NAME`
should be the address the Windows client will use to reach the host (its
LAN IP is fine, e.g. `192.168.1.50`).

**2. First run:** browse to `https://<docker-host>:8443`, accept the
self-signed cert warning, create the admin account. Go to **Clients →
Enroll new client** and copy the **Windows** command.

**3. Windows client (elevated PowerShell — works on Win 11 and Server):**
paste the command from the dialog. It downloads the agent, enrolls, and
installs the `CentralBackupAgent` service. The client appears under
**Clients** within seconds.

**4. Test:** open the client → **New job** → back up a specific folder
(e.g. `C:\Users\<you>\Documents` — start small, not all of `C:\`) →
**Back up now** → then open the snapshot and restore a file to a temp
folder to confirm.

Windows notes:
- Run the command in an **elevated** PowerShell (Run as Administrator);
  the installer checks and will tell you if it isn't.
- The service runs as LocalSystem, so it can read local files but **not**
  user-mapped network drives. Back up local paths (or UNC paths the
  machine account can reach).
- **Open/locked files** (a file held by an app, some profile/registry
  files) are retried then skipped and listed in the run log — the run is
  marked "partial", not failed. Consistent snapshots of locked files
  (VSS) are on the roadmap. For a clean first test, pick a folder without
  files currently open.
- The `CB_SERVER_NAME` value only needs to match for the *browser* to
  avoid an extra warning; the agent verifies the server by certificate
  **fingerprint**, not hostname, so it works even over a bare IP.
- If `Invoke-WebRequest`/`iwr` fails with *"The underlying connection was
  closed: An unexpected error occurred on a send"* on Windows PowerShell
  5.1, that's `.NET Framework` failing the TLS handshake — the server
  fixed this by using an RSA certificate and capping to TLS 1.2, which
  every client supports. If you're still hitting it, you're likely running
  an older server image; `git pull` and rebuild
  (`docker compose -f deploy/docker-compose.yml up -d --build --force-recreate`).
  Re-running `ensureTLSCert` only regenerates the certificate if none
  exists yet, so on an already-running server you may need to delete the
  `tls/` folder inside its data volume once to pick up the fix — this
  issues a new fingerprint, so grab a fresh enrollment token afterward.

## 0. Before you start

- Choose the machine that will **hold the backups** (the server). It needs
  disk space for all backup data and must be reachable by clients on one
  TCP port (default **8443**).
- Give it a stable address clients can reach — a DNS name
  (`backup.example.com`) or a static IP. This is the only inbound
  connectivity the whole system needs; clients connect outbound only.
- Open **inbound TCP 8443** to this machine (cloud security group / router
  port-forward / firewall). Nothing needs to be opened on the clients.
- Clients must reach that port **directly**. Do not put a Cloudflare Tunnel,
  Cloudflare's orange-cloud proxy, or any other TLS-terminating reverse
  proxy in front of the server — see
  [§3 Reverse proxies, tunnels and Cloudflare](#3-reverse-proxies-tunnels-and-cloudflare).

---

## 1. Install the server

### Option A — Docker (recommended)

On the server host, with Docker + Docker Compose installed:

```bash
git clone <this-repo> && cd <repo>
CB_SERVER_NAME=backup.example.com \
  docker compose -f deploy/docker-compose.yml up -d --build
```

- `CB_SERVER_NAME` must be the DNS name or IP clients will use — it is
  baked into the TLS certificate.
- All state lives in the `backup-data` Docker volume (`/data`): database,
  TLS certificate and the backup storage. Put a big disk there (or bind
  mount a path to `/data/storage`).
- Logs / fingerprint: `docker compose -f deploy/docker-compose.yml logs -f`.

### Option B — Portainer, pulling prebuilt images

Building the image compiles the Go server plus three cross-compiled agent
binaries. On a small VPS that is enough to exhaust RAM and get the build
killed by the OOM reaper. It also doesn't work if the stack was created
outside Portainer, which the UI marks as **limited** and refuses to edit.

Both are solved by building in CI and deploying by pull:

1. Push to GitHub. `.github/workflows/images.yml` builds and publishes
   `ghcr.io/<owner>/centralbackup-server` and `…-agent`.
2. Make the two packages **public** (GitHub → your profile → Packages →
   each package → Package settings → Change visibility), or on the host run
   `docker login ghcr.io -u <user> -p <PAT-with-read:packages>`.
3. In Portainer: **Stacks → Add stack → Web editor**, paste
   `deploy/portainer-stack.yml`, and set `CB_TAG`, `CB_OWNER`,
   `CB_SERVER_NAME` and `CB_PUBLIC_URL` under *Environment variables*.

To redeploy a new version afterwards: **Stacks → your stack → Update the
stack**, with *Re-pull image* enabled. Nothing is compiled on the host.

> **Volume names.** Compose prefixes volume names with the stack name, so
> deploying under a new stack name would create *empty* volumes — the server
> would start with no database, no certificate (a new fingerprint breaks
> every enrolled agent) and no backups. `deploy/portainer-stack.yml`
> therefore declares its volumes `external` with explicit names. Confirm
> yours match with `docker volume ls | grep backup` before deploying.

### Option C — Docker-free (systemd)

If you'd rather not use Docker (or can't pull images), the server is a
single static binary. With Go 1.25+ installed on the server host:

```bash
git clone <this-repo> && cd <repo>
sudo CB_SERVER_NAME=backup.example.com scripts/install-server.sh
```

This builds `backup-server` + the agent binaries, installs a
`backup-server` systemd service and starts it. Data goes to
`/var/lib/backup-server` by default (`CB_DATA_DIR` to change).

```bash
systemctl status backup-server
journalctl -u backup-server -f
```

---

## 2. First-run setup

1. Open `https://<server>:8443` in a browser.
2. The certificate is **self-signed** — your browser will warn. That's
   expected; proceed. (Clients don't rely on the browser's trust; they pin
   the certificate fingerprint — see below.)
3. Create the admin account on the first-run page.
4. Go to **Settings** and note the **TLS certificate fingerprint**. Clients
   verify the server against this value.

> Optional: to use a CA-issued certificate instead of self-signed (so the
> browser warning goes away), put your cert/key on the server and set
> `CB_TLS_CERT` / `CB_TLS_KEY`. Note that changing the certificate later
> changes the fingerprint and requires re-enrolling clients.

---

## 3. Reverse proxies, tunnels and Cloudflare

**Short version: don't put a TLS-terminating proxy in front of this
server.** Agents verify the server by pinning the SHA-256 fingerprint of
the certificate they are handed. Any middlebox that terminates TLS presents
*its own* certificate, so the pin fails and agents refuse to connect. This
is the intended behaviour — it is what makes a self-signed certificate safe
without a CA.

This rules out, for the address clients use:

- **Cloudflare Tunnel / `cloudflared`** — terminates TLS at Cloudflare's edge.
- **Cloudflare proxied DNS** (the orange cloud) — same.
- **nginx / Traefik / Caddy doing TLS termination** — same, unless you give
  the proxy the server's own certificate and key.

Using Cloudflare purely as a **DNS host is fine and recommended** — just set
the record to **DNS only (grey cloud)** so it resolves straight to your
server's IP.

### Symptoms of getting this wrong

| Symptom | Cause |
|---|---|
| `Client sent an HTTP request to an HTTPS server` | The proxy is speaking plain HTTP to the origin. This server is HTTPS-only — there is no HTTP listener. |
| Agent logs `server certificate fingerprint mismatch` | TLS is being terminated by something other than this server. |
| OAuth fails with `invalid_request` / redirect-URI mismatch | The redirect URI seen by the provider isn't the one registered. |

### Correct setup

1. **DNS**: an `A` record for your hostname pointing at the server's public
   IP, **not proxied**.
2. **Firewall**: inbound TCP 8443 open, both on the host and in your cloud
   provider's separate firewall/security-group layer.
3. **Verify from a machine outside the server** that TLS terminates on the
   server itself. The certificate's subject should be
   `CN=central-backup-server` (or your own cert, if you supplied one), and
   its fingerprint must equal the one the server logs at startup.

Linux/macOS:

```bash
dig +short backup.example.com                      # must be your server's IP
openssl s_client -connect backup.example.com:8443 </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -fingerprint -sha256
```

Windows PowerShell:

```powershell
Resolve-DnsName backup.example.com -Type A -Server 1.1.1.1 | Select-Object Name, IPAddress
Test-NetConnection backup.example.com -Port 8443

$h = 'backup.example.com'; $p = 8443
$tcp = New-Object Net.Sockets.TcpClient($h, $p)
$ssl = New-Object Net.Security.SslStream($tcp.GetStream(), $false, {$true})
$ssl.AuthenticateAsClient($h, $null, [Security.Authentication.SslProtocols]::Tls12, $false)
$cert = [Security.Cryptography.X509Certificates.X509Certificate2]$ssl.RemoteCertificate
"Subject : $($cert.Subject)"
$sha = [Security.Cryptography.SHA256]::Create().ComputeHash($cert.RawData)
"SHA-256 : " + ([BitConverter]::ToString($sha) -replace '-','').ToLower()
$ssl.Dispose(); $tcp.Close()
```

Compare against the server's own value (lowercase hex, no colons — the same
format both produce):

```bash
docker compose -f deploy/docker-compose.yml logs | grep -i fingerprint   # Docker
journalctl -u backup-server | grep -i fingerprint | tail -1              # systemd
```

### Set `CB_PUBLIC_URL`

Enrollment commands and the OAuth redirect URL both need the address
*clients and identity providers* will use. By default the server infers it
from the address you are browsing with, so opening the GUI by IP quietly
produces an enrollment command pointing at that IP. Pin it instead:

```bash
CB_SERVER_NAME=backup.example.com \
CB_PUBLIC_URL=https://backup.example.com:8443 \
  docker compose -f deploy/docker-compose.yml up -d
```

It must be `https://`, a bare origin with no path, and include the port
unless you serve on 443. The server refuses to start on a malformed value.

### If you genuinely can't expose a port

If the server has no public IP or inbound 8443 is blocked upstream, a
tunnel is not a workaround — pinning will still fail. The options are a
VPN/WireGuard link between clients and server (agents then connect to the
server's private address), or a proxy configured for **TCP passthrough**
(e.g. nginx `stream` with `proxy_pass`, no `ssl_certificate`), which
forwards the bytes without terminating TLS and so preserves the pin.

### Removing an existing tunnel

Do it in this order so the GUI stays reachable throughout: publish 8443 and
open the firewall first, confirm direct access works by IP, flip DNS to
grey-cloud, confirm again by hostname, and only then stop and delete the
tunnel. If the tunnel served on 443, agents will have `https://host` with no
port stored in their `creds.json`; patch the `server_url` field in place and
restart the agent — the `agent_id`, `secret` and `fingerprint` are unchanged,
so this is not a re-enrollment.

```bash
# containerized agent
docker exec backup-agent sh -c \
  "sed -i 's|https://backup.example.com\"|https://backup.example.com:8443\"|' \
   /var/lib/backup-agent/creds.json"
docker restart backup-agent
```

The same file lives at `/var/lib/backup-agent/creds.json` on Linux and
`C:\ProgramData\BackupAgent\creds.json` on Windows.

### A note on certificates

Don't delete `cert.pem` to regenerate it with a different hostname.
`CB_SERVER_NAME` only affects the browser's hostname warning — which a
self-signed certificate triggers anyway — while a new certificate means a
new fingerprint and **every enrolled agent stops connecting** until it is
re-enrolled. If you want a browser-trusted certificate, supply a real one
via `CB_TLS_CERT` / `CB_TLS_KEY` so TLS still terminates on this server, and
plan a re-enrollment window for each renewal.

---

## 4. Enroll clients

In the GUI: **Clients → Enroll new client**.

### Recommended: the downloadable installer

Download the installer for the client's platform. Each download carries its
own single-use token (valid 24 h) along with the server address and
certificate fingerprint, so nothing has to be copied onto the client:

```
Windows (elevated PowerShell):  .\backup-agent-installer.exe install
Linux:                          chmod +x backup-agent-installer
                                sudo ./backup-agent-installer install
```

This enrolls the client and installs the background service in one step —
`CentralBackupAgent` on Windows, `backup-agent.service` under systemd, or a
launchd daemon on macOS. Undo it with `backup-agent uninstall`, adding
`--purge` to delete the credentials and local job config too.

Because there is no bootstrap script, this avoids the whole class of
failures that come from the *installer* rather than the agent: PowerShell
version differences, .NET TLS negotiation, execution policy, and truncated
or mis-quoted tokens.

### Alternative: a ready-to-paste command

The same dialog shows a per-platform command that downloads and enrolls the
agent, embedding the server address, token and fingerprint.

### Ubuntu / Linux

```bash
curl -fsSLk https://<server>:8443/static/install-agent.sh | sudo bash -s -- \
  --server https://<server>:8443 --token <TOKEN> --fingerprint <FP>
```

Installs the agent, enrolls it, and runs it as the `backup-agent` systemd
service.

### Windows Server / Windows 11 (elevated PowerShell)

Use the exact command from the GUI's enrollment dialog (**Clients → Enroll
new client**) — it's generated fresh with the current token and
fingerprint, and is kept up to date with the compatibility fixes below.
The gist of what it does:

```powershell
Set-ExecutionPolicy -ExecutionPolicy Bypass -Scope Process -Force
# ... TLS/cert-trust setup for the self-signed cert (version-aware for
#     PowerShell 5.1 vs 7+) ...
iwr -UseBasicParsing -Uri https://<server>:8443/static/install-agent.ps1 -OutFile $env:TEMP\install-agent.ps1
& $env:TEMP\install-agent.ps1 -Server https://<server>:8443 -Token <TOKEN> -Fingerprint <FP>
```

Installs and starts the `CentralBackupAgent` Windows service. Notes from
real-world deployment:

- `Set-ExecutionPolicy -Scope Process` only affects the current PowerShell
  process (not the machine or user), but is required on any machine with
  the (common, often-default) `Restricted` policy — without it, running
  the downloaded `.ps1` fails with *"running scripts is disabled on this
  system"*.
- See the TLS handshake troubleshooting note in the quick-path section
  above if `Invoke-WebRequest` fails with *"An unexpected error occurred
  on a send"*.

### Docker host

Build the agent image once from the repo, then run it with the host
filesystem mounted read-only under `/host`:

```bash
docker compose -f deploy/agent-docker-compose.yml build
CB_SERVER=https://<server>:8443 CB_TOKEN=<TOKEN> CB_FINGERPRINT=<FP> \
  docker compose -f deploy/agent-docker-compose.yml up -d
```

Back up host paths as `/host/...` (e.g.
`/host/var/lib/docker/volumes/<vol>/_data`). Use a job **pre-hook** to
quiesce databases, e.g. `docker exec db pg_dump ... > /host-backup/db.sql`.

The client appears under **Clients** within a few seconds of enrolling.

---

## 5. Create a backup job and test

1. Open the client → **New job**. Set paths, optional exclude globs, a
   cron schedule (leave empty for manual only) and retention (e.g. keep
   last 30).
2. Click **Back up now** (Full). Watch it run under **Recent runs** — the
   run page shows live progress and a log.
3. Verify restore: open the resulting snapshot → **Browse / restore** →
   pick a file → **Download**, or restore a selection back to the client
   (choose an alternate directory to test safely).

Jobs can also be managed on the client itself and will sync back to the
GUI:

```bash
backup-agent job add --name docs --path /home/alice/docs \
  --schedule '0 2 * * *' --keep-last 30
backup-agent job list
```

---

## 6. Day-2

- Retention + cleanup runs daily; trigger it manually from **Settings →
  Run retention + cleanup now**.
- Back up the server itself by snapshotting its data directory
  (`/data` in Docker, `/var/lib/backup-server` for systemd).
- See [operations.md](operations.md) for logs, storage layout and
  troubleshooting, and [security.md](security.md) for the trust model.

---

## Verifying without real clients (smoke test)

To prove a fresh build works before touching production, run the bundled
end-to-end test — it starts a real server + agent locally and exercises
enroll, full/incremental backup, restore, config sync and GC:

```bash
scripts/e2e-test.sh
```
