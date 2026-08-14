# Backing up a Docker host

On a Docker host the backup agent runs **as a container**. It connects
outbound to the central server like any other client; the only differences
are *how it sees host data* (the host filesystem is mounted read-only at
`/host`) and *how it quiesces databases* (the Docker socket is mounted so
pre-hooks can run `docker exec`).

The quickest route is the **container picker** in the job editor (section
3): tick the containers you want and the paths and database dump hooks are
generated for you. The rest of this guide covers configuring the same jobs
by hand, and what each recipe is doing.

---

## 1. Build the agent image

On the Docker host, from a clone of this repo:

```bash
docker compose -f deploy/agent-docker-compose.yml build
```

This produces the `centralbackup/agent` image.

## 2. Enroll the host as a client

1. In the server GUI: **Clients → Enroll new client**, and copy the
   **token** and **fingerprint** from the *Docker host* section.
2. Start the agent (paste in your values; use your server's URL):

```bash
CB_SERVER=https://YOUR-SERVER:8443 \
CB_TOKEN=<token> \
CB_FINGERPRINT=<fingerprint> \
CB_NAME=docker-host \
docker compose -f deploy/agent-docker-compose.yml up -d
```

The container enrolls on first run, saves its credentials to the
`backup-agent-data` volume, and connects. Verify:

```bash
docker logs backup-agent          # "enrolled" then "connected to ..."
```

It should appear as **online** under **Clients** within a few seconds. The
compose file already mounts:

- `/:/host:ro` — the host filesystem, read-only, at `/host`
- `/var/run/docker.sock` — so hooks can run `docker exec`
- `backup-agent-data:/var/lib/backup-agent` — persistent identity + a
  writable staging area for database dumps

---

## 3. Pick containers in the GUI (easiest path)

Once the agent is connected, open the client → **New job**. If the client is
a Docker host, the editor shows a **Docker containers on this client**
panel listing every container, grouped by compose stack. Tick what you want
and the job's paths — and, for databases, the dump hooks — are filled in for
you. Nothing below this section is required unless you prefer to configure
jobs by hand or want to understand what the picker generated.

The picker classifies each container and behaves accordingly:

| Shown as | What it does |
|---|---|
| **database — dumped** | Adds a pre-hook that dumps to `/var/lib/backup-agent/staging/` and backs up that file, never the live data volume. MySQL/MariaDB, PostgreSQL, MongoDB and Redis generate a complete hook; SQL Server generates a template with `DATABASE` to replace. |
| **app database — copied** | SQLite/BoltDB apps (Vaultwarden, n8n, Portainer, Gitea, …). Adds the volume paths, plus an optional *stop the container during backup* tick-box for a guaranteed-consistent copy. |
| **files** | Adds the volume and bind-mount paths directly. |
| **no data to back up** | Greyed out — the container has no mounts, so there is nothing to capture. |

Notes:

- Paths are translated automatically: a containerised agent gets
  `/host/...` prefixes, a native agent gets plain host paths.
- **One database per job.** A job carries a single pre-hook, so ticking one
  database locks out the others. Give each database its own job.
- The Docker socket mount is never offered as a path — it is a control
  channel, not data.
- A container whose image is known to store data in a specific directory
  (e.g. Portainer's `/data`) that **no mount covers** is flagged in red:
  that container is not persisting its state at all, and backing up its
  volume would silently capture nothing.
- The list is read live from the client each time the editor opens, so new
  containers appear without touching the server. **Refresh** re-reads it.
- The picker only appears for online clients with a reachable Docker
  socket; everyone else just sees the normal path fields.

Requirements: the agent needs the Docker socket (the compose file in step 1
already mounts it). It is read-only usage — the agent only calls
`GET /containers/json`.

## 4. Find what to back up (manually)

`/host` exists **only inside the agent container**. Over SSH you use the
real host path; in a job you prepend `/host`.

List every mount for every running container:

```bash
docker ps --format '{{.Names}}' | while read c; do
  echo "== $c =="
  docker inspect "$c" --format '{{range .Mounts}}{{.Type}}  {{.Source}} -> {{.Destination}}{{println}}{{end}}'
done
```

Find your compose/stack definitions:

```bash
find /opt /srv /root /home -maxdepth 3 -name 'docker-compose.y*ml' 2>/dev/null
```

Path translation:

| On the host (SSH) | In the backup job |
|---|---|
| `/opt/stacks/myapp` | `/host/opt/stacks/myapp` |
| `/var/lib/docker/volumes/<vol>/_data` | `/host/var/lib/docker/volumes/<vol>/_data` |
| `/srv/myapp/config` | `/host/srv/myapp/config` |

Verify a path resolves inside the agent before using it in a job:

```bash
docker exec backup-agent ls -la /host/opt/stacks/myapp
```

---

## 5. Non-database files job

Back up stack definitions, bind-mounted app dirs, and **non-database**
named volumes (static content, uploads, config) directly.

**Client → New job:**

- **Name:** `docker-files`
- **Paths:**
  ```
  /host/opt/stacks
  /host/var/lib/docker/volumes/<app-volume>/_data
  ```
- **Excludes:** `*.tmp`, `*.log`, `cache`, `node_modules`
- **Schedule:** e.g. `0 3 * * *`
- **Keep last:** `30`

**Do not** put a database's data volume (e.g. `mysql_data`) here — copying
live DB files risks an inconsistent, unrestorable backup. Databases get a
dump job instead (next section). The same applies to anything else with
live consistency needs (Redis persistence, Elasticsearch, etc.).

---

## 6. Database job (MySQL example)

Dump the database to a consistent `.sql` file with a **pre-hook**, then back
up that file. The dump runs *inside* the database container, so its password
comes from that container's own environment and is never stored in the job
config. It writes to the agent's writable staging folder (not `/host`,
which is read-only).

**Client → New job:**

- **Name:** `mysql`
- **Paths:**
  ```
  /var/lib/backup-agent/staging/mysql-all.sql
  ```
- **Pre-hook:**
  ```
  mkdir -p /var/lib/backup-agent/staging && docker exec mysql sh -c 'exec mysqldump -uroot -p"$MYSQL_ROOT_PASSWORD" --single-transaction --routines --triggers --events --all-databases' > /var/lib/backup-agent/staging/mysql-all.sql
  ```
- **Post-hook:**
  ```
  rm -f /var/lib/backup-agent/staging/mysql-all.sql
  ```
- **Schedule:** `0 2 * * *`
- **Keep last:** `30`

Notes:

- `--single-transaction` gives a consistent InnoDB snapshot **without
  locking** the live database. `--routines --triggers --events` capture
  stored procedures, triggers and scheduled events. `--all-databases`
  includes users/grants; swap for `--databases <name>` to target one DB.
- If the container's password env var isn't `MYSQL_ROOT_PASSWORD`, change
  the `-p"$..."` to match (`docker exec mysql printenv | grep -i MYSQL`).
- The `[Warning] Using a password on the command line…` line goes to
  stderr, so it appears once in the hook log but is **not** written into the
  dump file. Append `2>/dev/null` after the closing quote to silence it.

The same pattern works for other engines — e.g. PostgreSQL:

```
docker exec db sh -c 'exec pg_dump -U "$POSTGRES_USER" "$POSTGRES_DB"' > /var/lib/backup-agent/staging/pg.sql
```

### SQL Server (Microsoft SQL / Express)

SQL Server can't stream a backup to stdout — `BACKUP DATABASE` writes a
`.bak` **file**. Point it at the database's own volume and back that file up
via `/host` (read-only reads are fine). `mssql-tools18`'s `sqlcmd` requires
`-C` to trust the self-signed certificate. Backup **compression is not
available in Express edition** — omit it (the agent's zstd compresses the
`.bak` anyway).

- **Paths:** `/host/var/lib/docker/volumes/<mssql-volume>/_data/backup/<db>.bak`
- **Pre-hook:**
  ```
  docker exec <container> mkdir -p /var/opt/mssql/backup && docker exec <container> bash -c '/opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -P "$MSSQL_SA_PASSWORD" -C -Q "BACKUP DATABASE [<db>] TO DISK='\''/var/opt/mssql/backup/<db>.bak'\'' WITH INIT, FORMAT"'
  ```
  `WITH INIT` overwrites the same file each run, so no post-hook cleanup is
  needed. Repeat the `BACKUP DATABASE` for `master`/`msdb` if you want
  logins and SQL-Agent jobs for full disaster recovery.
- **Restore:** `RESTORE DATABASE [<db>] FROM DISK='/var/opt/mssql/backup/<db>.bak' WITH REPLACE`.

### SQLite-backed apps (Vaultwarden, n8n, Portainer, …)

Apps that keep a live SQLite/BoltDB in a volume (Vaultwarden's
`db.sqlite3`, n8n's `.n8n/database.sqlite` — which also holds the critical
`encryptionKey` — Portainer's `portainer.db`) can be backed up by copying
the volume directly. A nightly hot copy is usually fine for low-write
services, but not strictly guaranteed consistent. For guaranteed
consistency give the service its own job with stop/start hooks (accept the
brief downtime; fine for Vaultwarden, think twice for n8n if it runs
scheduled workflows):

```
pre_hook:  docker stop <container>
post_hook: docker start <container>
```

---

## 7. Test and verify

1. **Back up now** on the job; check the run log shows the pre-hook running,
   `1 file` backed up, status **success**.
2. Open the resulting snapshot → **Browse / restore** and confirm the file
   is there (download it and check a MySQL dump starts with `-- MySQL dump`).

## 8. Restore

- **Files:** browse a snapshot and download, or restore to a writable
  directory. Because `/host` is mounted **read-only**, the agent can't write
  back into the host through it — restore to a download or a separate
  read-write path, or use the native (non-container) agent for a full
  in-place restore.
- **MySQL dump:** replay it into the database:
  ```bash
  docker exec -i mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD"' < mysql-all.sql
  ```

## Gotchas recap

- `/host` is **read-only** and exists only inside the agent container.
- Database **data volumes** are dumped, never raw-copied.
- Dump files go to `/var/lib/backup-agent/staging/` (writable), not `/host`.
- The Docker socket mount gives the agent control of the daemon — treat the
  agent container as privileged and keep the host secured.
