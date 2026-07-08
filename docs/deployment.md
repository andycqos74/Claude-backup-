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

### Option B — Docker-free (systemd)

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

## 3. Enroll clients

In the GUI: **Clients → Enroll new client**. This generates a one-time
token (valid 24 h) and shows a ready-to-paste command per platform. The
command embeds the server address, token and fingerprint.

### Ubuntu / Linux

```bash
curl -fsSLk https://<server>:8443/static/install-agent.sh | sudo bash -s -- \
  --server https://<server>:8443 --token <TOKEN> --fingerprint <FP>
```

Installs the agent, enrolls it, and runs it as the `backup-agent` systemd
service.

### Windows Server (elevated PowerShell)

```powershell
[Net.ServicePointManager]::ServerCertificateValidationCallback={$true}
iwr https://<server>:8443/static/install-agent.ps1 -UseBasicParsing -OutFile install-agent.ps1
.\install-agent.ps1 -Server https://<server>:8443 -Token <TOKEN> -Fingerprint <FP>
```

Installs and starts the `CentralBackupAgent` Windows service.

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

## 4. Create a backup job and test

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

## 5. Day-2

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
