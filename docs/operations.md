# Operations guide

## Server configuration (environment variables)

| Variable | Default | Purpose |
|---|---|---|
| `CB_LISTEN` | `:8443` | listen address |
| `CB_DATA_DIR` | `/data` (in Docker) | database, TLS cert, storage root |
| `CB_STORAGE_DIR` | `$CB_DATA_DIR/storage` | blob/manifest storage |
| `CB_SERVER_NAME` | – | extra SANs for the generated TLS certificate |
| `CB_TLS_CERT` / `CB_TLS_KEY` | – | bring your own certificate |
| `CB_AGENT_BIN_DIR` | `/app/agents` | prebuilt agent binaries served at `/dl/` |

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
