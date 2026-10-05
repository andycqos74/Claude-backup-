# Central Backup — marketing copy

## Homepage summary

> **Central Backup** is a self-hosted backup system that protects your Windows, Linux and Docker machines from one web dashboard, with no VPN or inbound ports required. It runs secure, deduplicated full and incremental backups to local disk, S3-compatible storage, OneDrive or Google Drive, and lets you restore files or run commands remotely in a few clicks.

---

## Product page

### Hero

**Back up every server from one screen, without opening a single port.**

Central Backup is a self-hosted backup system you control from a web dashboard. You install a small agent on each Windows, Linux or Docker machine. The agent connects out to your server, so you don't need a VPN, firewall changes or inbound ports on the clients.

**[Get started]** · **[Read the docs]**

### The problem

Backing up a mix of machines usually means a different tool on each box, scripts nobody remembers writing, and a VPN or port-forward for every remote site. When something breaks, you find out at restore time.

Central Backup puts all of it on one server, behind one port, in one dashboard.

### How it works

**1. Run the server.**
Start it with one Docker Compose command, or install it as a single binary with systemd. Open the web interface and create your admin account.

**2. Enroll your clients.**
Click *Enroll new client* and download the installer for that platform. The server address, a one-time token and the certificate fingerprint are already built into the installer, so there's nothing to copy. Run it once and the machine is protected.

**3. Set it and forget it.**
Define jobs, schedules and retention in the dashboard. Backups run on time, and you can watch them progress live.

### Features

#### No inbound ports, no VPN
Agents make outbound TLS connections to your server and keep a control channel open. The server can then start backups, restores and job changes on demand, even on machines behind NAT or at remote sites. Your server only needs to expose one port.

#### Full and incremental backups
Incremental runs read only the files that changed and upload only data the server doesn't already have. Every snapshot is still complete on its own, so a restore never has to rebuild a chain of incrementals.

#### Built-in deduplication and compression
Data is stored by its SHA-256 hash and compressed with zstd. Identical data is stored once, even across different snapshots and different machines.

#### Security built in
- Agents pin your server's certificate fingerprint, so a self-signed certificate is safe to use.
- Each client enrolls with a one-time token.
- Every block of data is hash-checked on upload and again on restore.

#### Manage jobs from the dashboard or the client
Edit a job in the web interface, or on the machine itself with `backup-agent job add` or `agent.yaml`. Changes sync both ways automatically.

#### Flexible scheduling
Set a cron schedule for each job, click *Back up now* whenever you need to, and have missed runs catch up when an offline machine comes back online.

#### Retention that cleans up after itself
Keep the last N snapshots, or everything newer than N days. Old data is garbage-collected automatically.

#### Works with Docker
The job editor shows a host's containers as tick-boxes, grouped by Compose stack. It fills in the volume paths for you, and for databases it recognises it also writes the dump hook, such as `pg_dump`.

#### Store backups where you want
Use the server's local disk, any S3-compatible provider (Backblaze B2, Wasabi, Cloudflare R2, MinIO, Storj), or your own OneDrive or Google Drive, which you connect from the dashboard.

#### Browse, restore and download
Look through any snapshot, then restore files to the original machine or download them straight from your browser.

#### Remote file transfer
Browse a client's filesystem from the dashboard and send files to it or pull files from it. Transfers run quietly in the background, are checked end to end, and wait in a queue if the machine is offline.

#### Remote command console
Run PowerShell, cmd or sh commands on any client and watch the output stream back live. You can cancel a command at any time, and doing so stops every process it started.

### Supported platforms
**Windows Server** (including legacy Windows 7-era systems) · **Ubuntu and Linux** · **macOS** · **Docker hosts**

### Your data stays yours
Central Backup is self-hosted. Your backups go only to storage you choose, on hardware or accounts you control. There's no third-party service in the middle and no lock-in.

### Closing call to action

**Protect every machine you manage in an afternoon.**
Run one server, install one agent per machine, and handle every backup from one dashboard.

**[Deploy Central Backup]**
