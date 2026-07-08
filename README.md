# Central Backup

A self-hosted, centrally controlled backup system with a web GUI. A small
agent runs on each client (Windows Server, Ubuntu/Linux, Docker hosts) and
connects **outbound** to the central server over TLS — no inbound ports, no
VPN and no firewall changes on clients. Backups are **full or incremental**,
compressed and deduplicated, stored on the server's local storage.

```
┌────────────┐  wss/https (outbound only)   ┌──────────────────────────┐
│ Windows    │ ───────────────────────────▶ │  Central server (Docker) │
│ agent      │                              │  • web GUI + REST API    │
├────────────┤ ───────────────────────────▶ │  • scheduler             │
│ Ubuntu     │                              │  • snapshot metadata     │
│ agent      │ ───────────────────────────▶ │  • blob storage (local)  │
├────────────┤                              └──────────────────────────┘
│ Docker host│      agents keep a persistent control channel;
│ agent      │      the server pushes "back up now", restores, job edits
└────────────┘
```

## Features

- **Web GUI** for everything: enroll clients, define jobs, schedules,
  watch progress live, cancel a run in progress, browse snapshots, restore
  or download files.
- **Jobs configurable from both sides** — edit in the GUI *or* on the
  client (`backup-agent job add …` / edit `agent.yaml`); changes sync both
  ways automatically (last write wins).
- **Server-driven scheduling**: cron schedules per job, run **manually
  from the GUI** at any time, optional catch-up when an offline client
  reconnects.
- **Full + incremental** backups. Every snapshot is logically complete
  (restores never chain), but incrementals only read changed files and only
  upload content the server doesn't already have (content-addressed,
  SHA-256, zstd-compressed, deduplicated across snapshots and clients).
- **Secure by construction**: agents connect outbound over TLS and pin the
  server certificate's fingerprint (self-signed certs are safe);
  enrollment uses one-time tokens; every blob is hash-verified on upload
  and restore.
- **Retention** per job (keep last N / newer than N days) with automatic
  garbage collection.
- **Docker-aware**: containerized agent for Docker hosts with pre/post
  hooks (e.g. `pg_dump` before backup) and host-volume access.
- Pluggable storage backend: local disk today; Google Drive / OneDrive
  planned (see [docs/roadmap.md](docs/roadmap.md)).

## Quick start — server

Requirements: Docker + Docker Compose on the machine that will hold backups.

```bash
git clone <this repo> && cd <repo>
CB_SERVER_NAME=backup.example.com docker compose -f deploy/docker-compose.yml up -d --build
```

- Open `https://<server>:8443` (the certificate is self-signed — that's
  expected) and create the admin account on first visit.
- `CB_SERVER_NAME` should be the DNS name (or IP) agents will use.
- All state lives in the `backup-data` volume (`/data`): SQLite database,
  TLS certificate and backup storage. Mount a big disk there.
- Remote clients must be able to reach port 8443 on this machine — that is
  the **only** port the whole system needs.

Prefer not to use Docker? The server is a single static binary — run
`sudo CB_SERVER_NAME=… scripts/install-server.sh` for a systemd install
instead. Full step-by-step (server + clients + firewall/TLS) is in
[docs/deployment.md](docs/deployment.md).

## Quick start — clients

In the GUI: **Clients → Enroll new client**. That generates a one-time
token and shows a ready-to-paste command for each platform:

- **Ubuntu/Linux** — downloads the agent, enrolls, installs a systemd
  service (`backup-agent.service`).
- **Windows Server** — downloads the agent, enrolls, installs the
  `CentralBackupAgent` Windows service (run in an elevated PowerShell).
- **Docker host** — runs the agent as a container with the host filesystem
  read-only under `/host` (see `deploy/agent-docker-compose.yml`).

The command embeds the server's TLS **fingerprint**, so the agent verifies
it is talking to the right server before trusting it.

## Defining backup jobs

From the **GUI**: open a client → *New job*. A job has paths,
exclude globs, a cron schedule (empty = manual only), "full every Nth run",
retention, optional pre/post hooks, and an enabled flag.

From the **client**:

```bash
backup-agent job add --name docs --path /home/alice/docs \
    --exclude '*.tmp' --schedule '0 2 * * *' --keep-last 30
backup-agent job list
backup-agent job rm docs
```

or edit `agent.yaml` (`/etc/backup-agent/agent.yaml` on Linux,
`C:\ProgramData\BackupAgent\agent.yaml` on Windows) directly. The agent
watches the file and syncs to the server; server-side edits are written
back into the file. Note: *removing* a job from the file does not delete it
— use `backup-agent job rm` or the GUI (this is deliberate, so a stale file
can't silently destroy configuration).

**Back up now**: on the client page, every job has *Back up now* /
*Full* buttons. If the client is offline the run is queued and starts when
it reconnects.

## Restore

Open a snapshot (client page → Snapshots → *Browse / restore*):

- **Restore to client** — select files/folders (or nothing = everything),
  choose the original location or an alternate directory, with or without
  overwrite. The server pushes the restore to the agent.
- **Download** — single files directly, or any selection as a **zip**,
  straight from the browser.

## Full vs incremental

- **Incremental** (default): files whose size+mtime are unchanged since the
  last snapshot are recorded without being re-read; only new/changed files
  are read, hashed and uploaded. Cheap enough to run nightly or hourly.
- **Full**: every file is re-read and re-hashed — use it periodically
  ("full every Nth run") to verify integrity. Even a full run only uploads
  content the server doesn't already have.

Storage is content-addressed: identical files across snapshots (and across
clients) are stored once.

## Repository layout

| Path | What |
|---|---|
| `cmd/server`, `cmd/agent` | binaries |
| `internal/server` | GUI, API, hub, scheduler, retention/GC |
| `internal/server/store` | SQLite metadata |
| `internal/server/storage` | storage backends (localfs) |
| `internal/agent` | connection, backup/restore engine, agent.yaml sync |
| `internal/proto` | shared protocol types |
| `deploy/` | Dockerfiles and compose files |
| `scripts/` | release build |
| `docs/` | security model, operations, roadmap |

## Development

```bash
go test ./...          # unit tests
go vet ./...
scripts/build-release.sh   # cross-compile server + agents into dist/
```

See [docs/security.md](docs/security.md) for the security model,
[docs/operations.md](docs/operations.md) for day-2 operations and
[docs/roadmap.md](docs/roadmap.md) for planned work (cloud storage
backends, VSS, client-side encryption).
