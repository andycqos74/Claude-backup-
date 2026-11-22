# Evaluation: Central Backup vs Kopia, and a Kopia + MeshCentral management platform

Status: design evaluation, November 2026. No code changes. Kopia facts are
for v0.23.x (latest release found: v0.23.1, June 2026). MeshCentral facts
are for the 1.2.x line. Anything marked **verify** came from memory or
secondary sources and should be checked against current upstream docs
before you build on it.

---

## 1. How this repo backs up today (from the code)

| Aspect | Implementation | Where |
|---|---|---|
| Unit of dedup | **Whole file.** The SHA-256 of the full file content is the blob key. | `internal/agent/engine.go` |
| Change detection | Incremental runs skip files whose size and mtime match the previous manifest. A full run re-reads and re-hashes everything. | `engine.go` |
| Compression | zstd at the library default level (about level 3), applied per file on the agent, always on. Manifests are zstd JSONL. | `engine.go:385`, `commitManifest` |
| Storage layout | One object per unique file (`blobs/ab/<sha>.zst`) and one object per snapshot (`manifests/<id>.jsonl.zst`). | `storage/storage.go` |
| Index | The snapshot → client/job/time mapping and the blob index live **only in SQLite**. | `store/` |
| Integrity | Blobs are hash-verified on upload and on restore. GC aborts if any manifest is unreadable. A 24 h grace period protects in-flight uploads. | `gc.go` |
| Interrupted runs | No snapshot is committed. Blobs already uploaded are kept, so a re-run skips them (resumable per file, not within a file). | `engine.go` |
| Encryption at rest | **None.** Blobs on S3/OneDrive/GDrive are plaintext zstd. Client-side encryption is roadmap item 5. | `docs/roadmap.md` |
| Windows open files | `SeBackupPrivilege` + `FILE_FLAG_BACKUP_SEMANTICS` gets past ACLs, but there is **no VSS**. Exclusively locked files are skipped, and the run is marked *partial*. | `backup_access_windows.go` |
| Restore metadata | Mode bits (Unix only), symlinks, directories. No ownership, xattrs or Windows ACLs. | `restore.go` |
| Control plane | Persistent outbound WSS hub with offline queueing, live progress, cancel, two-way job sync with `agent.yaml`, cron scheduling, catch-up, Docker picker, file transfer, remote command console, Windows scheduled-task management. | `internal/server/hub.go` etc. |
| Install | A single self-enrolling exe or ELF (enrollment JSON appended after the image), plus a Windows 7 build made with Go 1.20. | `internal/bundle`, `scripts/build-legacy-agent.sh` |

## 2. How Kopia backs up

- **Content-defined chunking.** A rolling-hash splitter (buzhash, about 4 MiB
  average by default) cuts files into chunks. When part of a large file
  changes, only the changed chunks are new.
- **Pack files.** Chunks are bundled into pack blobs of roughly 20–40 MB.
  Ten thousand small files become a handful of objects, not ten thousand.
  This matters a great deal on OneDrive, Google Drive and S3, where request
  counts and per-object overhead dominate.
- **Encryption is always on.** The repository is encrypted (AES-256-GCM or
  ChaCha20-Poly1305) and keys are derived from the repository password, so
  the storage provider never sees plaintext or file names.
- **Compression is off by default** and is set per policy: zstd (several
  levels), s2, pgzip, lz4 and others. There are per-extension and minimum-size
  rules so incompressible media can be skipped.
  (`kopia policy set --global --compression=zstd`.)
- **Index stored in the repository.** Snapshot manifests and indexes are
  objects in the repository. Point a fresh Kopia at the bucket, give it the
  password, and everything is browsable. No external database is needed.
- **Maintenance.** Quick and full maintenance with safety margins, explicit
  ownership of the maintenance lock, `snapshot verify` (it can re-read a
  percentage of content), and optional Reed–Solomon error correction
  (experimental).
- **Storage backends.** Filesystem, S3-compatible, B2, Azure, GCS, SFTP,
  WebDAV, Google Drive (native, but marked experimental and CLI-only), and
  rclone for everything else, including OneDrive.
- **Windows.** Native VSS through policy:
  `kopia policy set <target> --enable-volume-shadow-copy=when-available`
  (values: never, always, when-available, inherit). Before/after actions are
  also available.
- **Checkpoints.** Long snapshots write periodic checkpoints
  (`--checkpoint-interval`, at most 45 minutes), so an interrupted
  multi-hour first backup does not start from zero.
- **Repository server.** `kopia server start` fronts one repository for
  many clients over gRPC/HTTPS. Each client authenticates as `user@host`
  (`kopia server user add`), and storage credentials never leave the server.
  Clients pin the server's TLS fingerprint (`--server-cert-fingerprint`), the
  same model as this repo. Default ACLs give each user FULL access to its own
  snapshots and policies and APPEND-only access (read and write, no delete)
  to content, so a client **cannot delete** other clients' snapshots.
  Caveat from the docs: "Kopia does not currently perform access control
  checks" on object IDs, so a client that learns another client's object ID
  can read that data. Treat clients as mutually semi-trusted for
  confidentiality, though not for deletion. There is also an upstream forum
  thread titled "server mode is insecure (server must be trusted)". The
  server sees plaintext, which is fine when you run it yourself.
- **What Kopia does *not* have.** Fleet management. There is no push channel
  from the server to clients. Each client schedules itself (KopiaUI or a
  local `kopia server`). The repository server cannot tell a client "back up
  now", cannot restore onto a client, and does not know which clients are
  online.
- **Windows 7.** Kopia is built with a current Go toolchain, so it needs
  Windows 10 / Server 2016+. This repo's legacy Go 1.20 agent build has no
  Kopia equivalent.

## 3. Verdicts

### Robustness: **Kopia**

- The repository is self-describing. If you lose this repo's SQLite
  database, the manifests are orphaned. They are named only by snapshot ID,
  and `proto.ManifestEntry` holds only path, size, mode, mtime and hash, with
  no client, job or time. The blob store becomes a pile of hashes. Losing
  Kopia's local config costs nothing.
- The cost on Kopia's side: the **repository password is unrecoverable**.
  Lose it and every backup is gone. Store it in Vaultwarden on day one.
- Encryption and authenticated content mean a tampered or corrupted object
  is detected and cannot be read by the storage provider.
- VSS means consistent Windows backups of open files. This repo skips them.
- Kopia's maintenance has years of field hardening across many users
  (including the v0.23.1 race fix). This repo's GC is careful (abort on an
  unreadable manifest, grace window, commit lock) but has one user base.
- Where this repo is better: end-to-end hash verification is simple and easy
  to audit, and "no snapshot on cancel" semantics are clear.

### Features (backup engine only): **Kopia**

Kopia adds chunk-level dedup, encryption, VSS, a portable repository, many
more storage backends, policy inheritance (global → host → path), richer
retention (hourly/daily/weekly/monthly/annual), mounting snapshots as a
filesystem, and snapshot verification.

### Features (management / operations): **this repo, by a long way**

Kopia has nothing like the hub. The hub provides server-pushed runs, offline
queueing, live progress, restore-to-client, job editing from both sides,
Docker container picker with dump-hook generation, remote shell, file
transfer, Windows scheduled-task control and self-enrolling installers.
These are the parts worth keeping.

### Compression efficiency: **Kopia (once compression is enabled)**

- Per-byte compression is roughly equal: both use zstd, at similar levels if
  Kopia is set to `zstd`. Kopia's default (no compression) is *worse*, so
  enabling it in the global policy is mandatory.
- Storage efficiency over time is where Kopia wins clearly. Content-defined
  chunking means:
  - A 20 GB VM image, PST/OST, SQL `.bak` or `.mdf` with a small change costs
    megabytes per snapshot, not 20 GB. This repo re-uploads the whole file
    (roadmap item 9).
  - Appended logs and partially edited archives dedupe across versions.
  - Packing removes per-object overhead for small files.
- For a dataset of many small, unchanging documents, the two are close.

Not measured. The download of the Kopia binary for a side-by-side test was
blocked in this environment. A quick test worth running yourself: one
directory with text, duplicates, an incompressible file and one large file;
snapshot it with both tools, change 1 MB in the middle of the large file,
snapshot again, and compare stored bytes after each run.

## 4. Feasibility: central management with Kopia as the backup engine

**Feasible, and the most sensible route is evolution rather than a rewrite.**
Keep this repo's server and agent as the control plane. Replace only the
engine (`engine.go`, `restore.go`, server-side blob storage, manifests and
GC) with Kopia.

### 4.1 Who drives Kopia on the client: **the existing Go agent**

Options:

| Option | Assessment |
|---|---|
| **A. Agent embeds Kopia as a Go library** (`repo`, `snapshot/snapshotfs`, `snapshot/restore`), pinned to a version | **Recommended.** One binary per platform, so the single-exe installer survives. Progress comes from Kopia's `UploadProgress` callbacks straight into the existing `runLog` and progress messages. No child process to supervise. Cost: Kopia's internal Go API is not a stable public API, so each Kopia upgrade needs a small adaptation pass and a re-run of the e2e test. |
| B. Agent shells out to a bundled `kopia` binary with `--json` | Acceptable fallback. The CLI is more stable than the Go API, but it means two binaries to ship and update and progress parsing from stdout. |
| C. MeshCentral agent runs `kopia` commands | Rejected. No structured results, no offline queue, no progress, no job sync. You would rebuild the hub badly on top of a remote-shell feature. |

What changes in the agent: `doBackup` becomes "open repository connection →
apply job → policy → `snapshotfs.Uploader.Upload` → report manifest ID". Restore
becomes a Kopia restore of the selected paths. Hooks, cancel, partial status
and the hub protocol stay as they are.

### 4.2 Repository topology: **central Kopia repository server in the stack**

- Run `kopia server` as a container next to the controller. It owns the
  storage credentials (local disk, B2, R2, Wasabi, and so on).
- The controller creates one Kopia user per enrolled client
  (`kopia server user add <client-id>@<host>`) and pushes the password to the
  agent over the existing authenticated hub. That replaces today's
  agent-secret upload auth.
- ACLs keep each client to its own snapshots, so a compromised client cannot
  delete other clients' backups. Clients writing straight to the bucket would
  each need credentials capable of deleting the whole repository, which is
  the wrong trade for a fleet.
- The controller opens the same repository read-only to list snapshots, browse
  and stream browser downloads (zip). That replaces `manifest.go`.
- Retention moves to Kopia policies (one per `client@host:path`), and the
  controller writes them from the job editor. Maintenance runs on the server
  as the single maintenance owner.
- Cost: data still flows client → server → cloud, as today (roadmap item 2
  stays unsolved). Accept that for now. If the server uplink becomes the
  bottleneck, give large clients direct-to-bucket repositories with
  restricted keys later.
- For 3-2-1: `kopia repository sync-to` mirrors the repository to a second
  backend (roadmap "off-site mirror", nearly for free).

### 4.3 What the new engine loses

- **Windows 7 / POSReady 7 clients.** Kopia (and Tailscale) need Windows 10
  or later. Options: keep the current engine in the legacy build only (the
  hub protocol is the same, so the server can support both engines during
  migration), or retire those machines. MeshAgent still supports Windows 7,
  so remote access for them keeps working either way.
- Existing snapshots. There is no in-place conversion. Run both engines side
  by side and let the old snapshots age out under retention.
- The OneDrive OAuth backend. Kopia reaches OneDrive only through rclone.
  Its native Google Drive backend is experimental. Prefer an S3-compatible
  target (B2, R2, Wasabi) for the repository.
- Zero-knowledge storage. Kopia encrypts against the *storage provider*,
  but in the central-server topology the server holds the key and sees
  plaintext, so roadmap item 5 (the server never sees plaintext) is
  **not** delivered.

## 5. MeshCentral: remote desktop, terminal and files

MeshCentral (Node.js, self-hosted, Apache 2.0) provides remote desktop,
terminal, file manager, power actions, Intel AMT, device groups, users and 2FA.
Its own agent (MeshAgent, written in C) supports Windows 7+ and most Linux
systems, and the server generates the installers.

**Recommended role: remote access only, embedded in the controller UI. Not
the backup transport.**

- **Integration.** Enable `allowLoginToken` (or use a dedicated service
  account) and have the controller mint short-lived login tokens. Each client
  page then shows *Desktop*, *Terminal* and *Files* buttons, opening
  MeshCentral at that node in an iframe or a new tab (`viewmode` URL
  parameters select the tab for that node). **Verify** current token
  lifetime and embedding settings. A user report says tokens last one hour.
- **Mapping.** The controller stores each client's MeshCentral node ID. Get
  it by having the backup agent report the local MeshAgent's node ID, or by
  matching hostname through `meshctrl` / the MeshCentral websocket control
  API.
- **SSH.** MeshCentral's terminal gives a shell through the agent with no
  inbound port and no SSH server needed. MeshCentral also has a web SSH
  client (relayed through an agent) for devices that only run `sshd`.
- **Overlap.** MeshCentral replaces this repo's remote command console and
  file transfer for interactive use. Keep them anyway: they are scriptable,
  audited per run and work on clients without MeshAgent. The Docker picker
  and scheduled-task management have no MeshCentral equivalent; keep them.

**Connection mechanism?** Do not route backups through MeshCentral relays.
Relays are designed for interactive sessions, not multi-GB transfers, and the
backup hub already gives you an outbound-only connection. Two agents on a
client is normal (RMM products do the same).

## 6. Networking: Cloudflare Tunnel and Tailscale

There are three kinds of traffic. Keep them apart.

| Plane | Traffic | Path |
|---|---|---|
| **Admin** | Controller GUI, MeshCentral web UI, Kopia server UI (optional) | **Cloudflare Tunnel + Cloudflare Access** (SSO/OTP policy). No public port. |
| **Agent control** | Backup hub WSS, MeshAgent WSS | Direct port on the server, *or* over the tailnet (see below). |
| **Data** | Kopia gRPC (snapshot upload and restore) | Same as agent control. **Never through the tunnel.** |

Why not put everything behind the tunnel:
- The backup agent and Kopia clients pin the server's TLS fingerprint.
  Cloudflare terminates TLS, so pinning fails (already documented in
  `docs/deployment.md` §3). Pinning against a TLS-terminating proxy cannot
  be fixed without weakening it.
- Cloudflare's request-body limits (100 MB on Free/Pro) and its terms on
  bulk non-web traffic make it a poor transport for backup data, and
  long-lived streams get cut.
- MeshCentral is **not** an exception. MeshCentral's reverse-proxy docs say
  agents authenticate the server through the *web TLS certificate*. Behind
  an offloading proxy, MeshCentral must be told (`certUrl`) which
  certificate agents will see. Cloudflare's edge certificates are managed and
  rotated by Cloudflare, so agents connecting through the tunnel would break
  on rotation. Have MeshCentral terminate its own TLS. Point agents directly
  at it on a published port with grey-cloud DNS. Point `cloudflared` at the
  same HTTPS origin (`noTLSVerify` or the origin certificate) for the admin
  UI only. **Verify**: whether MeshCentral supports a separate agent-only
  port (`agentPort`) in your version. The docs page fetched lists
  `AliasPort`, but `agentPort` was not confirmed.

**Tailscale (or self-hosted Headscale): optional, and useful in one case.**
- *Without it:* publish one port for agents (the hub plus Kopia gRPC can share
  a host on two ports, or be multiplexed behind one TCP passthrough). Clients
  on the WAN connect outbound. That is how the repo works today, and it is
  enough.
- *With it:* the server publishes **no** ports at all. Agents connect to the
  server's tailnet IP, so pinning works with no public exposure.
  Tailscale/Headscale handle NAT traversal and key distribution for many
  machines, which hand-run WireGuard (your current setup) does not scale to.
  Tailscale SSH and subnet routers also reach non-agent devices on a client's
  LAN (printers, NAS, switches).
- *Costs:* a third agent on every client, auth keys to embed in the
  installer, no Windows 7, and the tailnet becomes a dependency of backups.
  MeshCentral already provides relayed remote access, so Tailscale adds little
  there.
- **Recommendation:** start without it. Add Headscale (self-hosted, fits the
  Docker stack) later if you want zero public ports, and make the installer
  join the tailnet with a pre-auth key only on sites where that is wanted.

## 7. The Docker stack

Following your conventions (segmented networks, nothing published that
does not need to be, secrets in Vaultwarden):

```
networks:  tunnel (cloudflared ↔ web UIs), internal (controller ↔ kopia ↔ mesh ↔ mongo),
           egress (kopia → S3, controller → OAuth), agents (published ports only)

services:
  cloudflared        tunnel → controller:8443(ui), meshcentral:443         [tunnel]
  controller         this repo's server: GUI, hub, scheduler, installer
                     builder; publishes the agent WSS port                  [tunnel, internal, agents, egress]
  kopia-server       kopia server start --tls-*; repository on /repo or S3;
                     publishes the gRPC port                                [internal, agents, egress]
  meshcentral        own TLS; agent port published, UI via tunnel origin    [tunnel, internal, agents]
  mongodb (optional) MeshCentral uses NeDB by default; Mongo for larger fleets [internal]
  headscale (later)                                                         [agents]
```

**Required change in this repo first.** Today the server serves GUI, admin
API and agent API from one mux on one listener (`internal/server/server.go`,
`web.go`), and `CB_PUBLIC_URL` is one value used for both enrollment
commands and the OAuth redirect. Publishing "the agent port" would therefore
publish the admin login as well. Split it:

- A second listener (e.g. `:8444`) that serves only `/api/agent/*` and `/dl/*`,
  with the pinned self-signed cert. Publish that one.
- The existing listener for GUI and admin API, unpublished and reached only by
  `cloudflared` on the internal network.
- Two settings: an agent URL (baked into installers) and an admin URL (OAuth
  redirect, links).

MeshCentral needs the same care: if its UI and agent endpoint share a port,
publishing it for agents also publishes the login page. Keep MeshCentral 2FA
on, and restrict the published port by source IP where sites are known.

The admin hostnames (e.g. `backup.example.com`, `mesh.example.com`) go through
the tunnel behind Access. One agent hostname
(`agents.example.com`, grey-cloud DNS) is published directly: hub, Kopia gRPC
and MeshAgent ports. On the Fasthosts VPS, open those ports in **both** the
cloud-panel firewall and ufw, and remember Docker publishes bypass ufw.

## 8. Client install: one exe for Windows, one command for Linux

Extend the existing self-enrolling bundle into a bootstrapper:

1. The admin clicks *Enroll* and downloads `backup-agent-installer.exe` (or
   runs `curl … | sudo sh` on Linux), as today.
2. The installer enrolls with the controller (existing token and fingerprint
   flow) and installs the backup agent service. With option A, Kopia is
   inside that binary.
3. The controller's enrollment response includes a MeshCentral installer URL
   for the right device group (MeshCentral serves agent downloads with the
   group's `.msh` config baked in). The agent downloads it and installs it
   silently (`-fullinstall`). It does the same for Tailscale with a pre-auth
   key if that site uses one.
4. The agent reports its MeshCentral node ID back, so the controller links
   the two automatically.

Things to watch:
- **Code signing.** Appending the enrollment JSON after the PE image breaks an
  Authenticode signature (the signature would not cover the trailing data
  and strict verifiers flag it). If you sign, move the enrollment into an
  unauthenticated signature attribute or the certificate-table padding (the
  technique MeshCentral itself uses), or ship a signed MSI and a tiny config
  file.
- **Antivirus.** MeshAgent is regularly flagged as a remote access tool or
  PUA by Defender and other products, because attackers abuse it too. Expect
  to add exclusions or allow-list it in managed AV. Signing your own installer
  helps with SmartScreen but not with MeshAgent's reputation.
- **Updates.** The backup agent needs a self-update path (there is none
  today). MeshAgent self-updates from its server.

## 9. Recommended path

| Phase | Work | Result |
|---|---|---|
| 0 | Run the compression/dedup test from §3 on real data, including one large DB or PST. Confirm Kopia's VSS flag and repository-server ACL behaviour on a Windows 10 test box. | Evidence to justify the switch. |
| 1 | Split the controller into agent and admin listeners with two URLs (§7). Add MeshCentral to the stack and put the UIs behind the tunnel and Access. Install MeshAgent from the bootstrapper. Add Desktop/Terminal buttons on the client page. | Remote access in one console. No engine risk. |
| 2 | Add the Kopia server container. Add a second engine to the agent (option A) chosen per job. The controller provisions Kopia users and policies and lists Kopia snapshots alongside existing ones. | Both engines run in parallel. New jobs use Kopia. |
| 3 | Port restore (to client, and browser download) to Kopia. Make Kopia the default engine. Set up `sync-to` for an off-site mirror. Keep the legacy engine only for the Windows 7 build. | Old snapshots age out, and engine code shrinks. |
| 4 (optional) | Headscale for zero public ports. Code-signed installer. Agent self-update. | Hardening. |

Rough size: phase 1 is days. Phases 2–3 are a few weeks for one developer,
mostly in the agent engine swap and the controller's snapshot and restore
views. The hub, scheduler, GUI, enrollment and Docker features carry over
unchanged.

## Sources

- [Kopia releases](https://github.com/kopia/kopia/releases)
- [Kopia repository server](https://kopia.io/docs/repository-server/)
- [Kopia repositories / backends](https://kopia.io/docs/repositories/)
- [kopia policy set reference](https://kopia.io/docs/reference/command-line/common/policy-set/)
- [kopia snapshot create reference](https://kopia.io/docs/reference/command-line/common/snapshot-create/)
- [Kopia compression docs](https://kopia.io/docs/advanced/compression)
- [Kopia features](https://kopia.io/docs/features/)
- [Kopia FAQ](https://kopia.io/docs/faqs/)
- [Kopia forum: Windows 7](https://kopia.discourse.group/t/kopia-for-windows-7-recommendations/2061)
- [Kopia forum: VSS support](https://kopia.discourse.group/t/how-to-activate-vss-on-existing-upgraded-versions/2852)
- [MeshCentral docs](https://docs.meshcentral.com/meshcentral/)
- [MeshCentral releases](https://www.github.com/Ylianst/MeshCentral/releases)
- [MeshCentral login-token issue](https://github.com/Ylianst/MeshCentral/issues/718)
- [Syncthing forum: Go and Windows 7](https://forum.syncthing.net/t/end-of-windows-7-64bits-support/26137)
