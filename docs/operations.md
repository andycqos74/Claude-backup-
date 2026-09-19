# Operations guide

## Server configuration (environment variables)

| Variable | Default | Purpose |
|---|---|---|
| `CB_LISTEN` | `:8443` | listen address |
| `CB_DATA_DIR` | `/data` (in Docker) | database, TLS cert, storage root |
| `CB_STORAGE_DIR` | `$CB_DATA_DIR/storage` | blob/manifest storage |
| `CB_SERVER_NAME` | – | extra SANs for the generated TLS certificate |
| `CB_PUBLIC_URL` | – | externally reachable origin (e.g. `https://backup.example.com:8443`); pins the address used in enrollment commands and the OAuth redirect URL |
| `CB_TLS_CERT` / `CB_TLS_KEY` | – | bring your own certificate |
| `CB_AGENT_BIN_DIR` | `/app/agents` | prebuilt agent binaries served at `/dl/` |

## Agent configuration (environment variables)

| Variable | Default | Purpose |
|---|---|---|
| `CB_DOCKER_SOCKET` | `/var/run/docker.sock` | Docker Engine socket used to list containers in the job editor |

## Backups of the backup server

Snapshot the whole `/data` volume (SQLite db + `storage/`). The database
uses WAL mode; for a crash-consistent copy stop the container or use
filesystem snapshots.

## Storage layout

```
/data
├── server.db              # metadata (SQLite)
├── tls/cert.pem, key.pem  # server certificate (fingerprint = agent pin)
└── storage
    ├── blobs/ab/<sha256>.zst        # content-addressed file data
    └── manifests/<snapshot>.jsonl.zst
```

## Retention & garbage collection

- Per job: *keep last N snapshots* and/or *keep newer than N days*; the
  newest snapshot is always kept.
- Runs automatically once a day, or on demand via **Settings → Run
  retention + cleanup now**.
- Deleting a snapshot/client removes metadata and manifests immediately;
  blob data is reclaimed by the next GC pass.

## Agent operations

| Task | Linux | Windows |
|---|---|---|
| status | `systemctl status backup-agent` | `Get-Service CentralBackupAgent` |
| logs | `journalctl -u backup-agent -f` | Event Viewer (Application) |
| state dir | `/var/lib/backup-agent` | `C:\ProgramData\BackupAgent` |
| job config | `/etc/backup-agent/agent.yaml` | `C:\ProgramData\BackupAgent\agent.yaml` |
| uninstall | disable service, remove files | `backup-agent service uninstall` |

The agent state directory contains `creds.json` (agent identity — protect
it) and cached manifests (`manifest-<job>.jsonl.zst`) used to speed up
incrementals; deleting caches is safe, the agent refetches from the server.

## Sending a file to a client

A client's page has a **Send file to client** button (Clients → pick a
client). Choose a file and a destination path on that client, then send it.
The file is delivered over the same authenticated, fingerprint-pinned
channel the agent already uses, and written to disk by the agent's
background service — nothing is shown on the client's screen. Progress and
the final result are visible only here, in the server GUI.

- **Destination path.** End it with `/` or `\` (or point it at an existing
  folder) to drop the file into that folder under its own name; otherwise
  the path is the full target filename, e.g.
  `C:\ProgramData\app\config.yaml` or `/etc/app/config.yaml`.
- **Overwrite.** Off by default: if the target already exists the transfer
  fails rather than clobbering it. Tick *Overwrite* to replace it.
- **Offline clients.** The push is queued and delivered automatically when
  the client next reconnects.
- **Integrity.** The content is hashed on upload and re-verified on the
  client before the file is moved into place (an atomic rename), so a
  partial or corrupted transfer never leaves a half-written file.
- **Permissions.** The agent runs as LocalSystem (Windows) or root
  (systemd), so it writes with those privileges. On Windows the service can
  reach paths a normal user cannot; there is no interactive prompt.
- **Housekeeping.** The uploaded copy is kept on the server (in the active
  storage backend) until you *Remove* the transfer, so its status stays
  auditable and it can be re-sent. Removing a client also removes its
  parked payloads. Pushes are meant for configs, scripts and small
  installers (capped at 2 GiB); use a backup job for bulk data.

## Docker-host jobs

With the containerized agent, host paths appear under `/host` (read-only).
Typical jobs:

- named volumes: `/host/var/lib/docker/volumes/<vol>/_data`
- compose projects: `/host/srv/myapp`
- databases: add a pre-hook such as
  `docker exec mydb sh -c 'pg_dump -U app app > /var/lib/mydb-dump/app.sql'`
  and back up the dump path.

Because `/host` is read-only, restores on containerized agents must target
a writable directory (e.g. a volume you mount read-write), or run the
native agent instead.

## Troubleshooting

- **Client shows offline**: check agent service logs; verify the client can
  reach `https://server:8443`; fingerprint mismatches are logged explicitly.
- **Run failed with "agent disconnected"**: the connection dropped mid-run;
  the next scheduled run (or Back up now) simply continues — already
  uploaded data is reused thanks to content addressing.
- **Partial runs**: some files were skipped (locked/permission denied);
  the run log lists every one.
