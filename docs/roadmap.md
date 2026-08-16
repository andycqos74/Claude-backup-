# Roadmap

Planned enhancements, roughly in priority order.

1. **Cloud storage backends** — *OneDrive and Google Drive are done*
   (Settings → Backup storage backend). Remaining:
   - **Box** backend (needs Box's chunked-upload + SHA-1 commit protocol
     for objects over 50 MB; the provider is listed but disabled in the GUI
     until this lands).
   - **Off-site mirror**: keep local storage primary and asynchronously
     replicate to a cloud backend for 3-2-1 style copies (today a single
     backend is active at a time).
   - **Migration**: copy existing snapshots when switching backends (today
     a switch only affects new backups).
2. **Direct client → cloud transfer (server out of the data path)** — today
   backup data flows client → server → cloud, so the server's uplink is the
   bottleneck. Instead, keep the server as the control plane (auth, dedup
   decisions, blob index, manifests, scheduling) but have clients transfer
   bytes straight to the cloud provider:
   - **Upload**: the server creates a resumable upload session per missing
     blob and returns the provider's pre-authenticated upload URL to the
     agent (OneDrive and Google Drive sessions already return such URLs that
     need no credentials — the OneDrive backend already uses this). The
     agent uploads directly; no cloud credentials ever reach clients.
   - **Restore**: the server mints short-lived download URLs for the agent
     to pull blobs directly.
   - Trade-offs to resolve: content-hash verification moves to client trust
     (mitigate with post-upload checks); clients then also need outbound
     HTTPS to the provider, not only to the server; upload-session support
     varies by provider (clean for OneDrive/Drive, awkward for Box).
   - Likely a per-provider capability with automatic fallback to the current
     proxied path when direct transfer isn't available.
3. **Real-time / continuous backup** — the agent watches a job's folders
   (fsnotify: inotify on Linux, ReadDirectoryChangesW on Windows) and, after
   a short debounce of quiescence, pushes changes as an incremental — closer
   to one-way sync than scheduled backups. Design notes:
   - *Phase 1 (reuses the engine):* a per-job `realtime` flag; on debounced
     change, trigger the existing incremental for that job. Near-real-time
     (changes captured seconds after they settle) with minimal new code.
   - *Phase 2:* targeted "dirty-set" backups that process only the changed
     paths and merge them into the previous manifest, avoiding a full tree
     re-walk on large jobs.
   - *Correctness safety net:* watchers can drop events under bursts
     (inotify `IN_Q_OVERFLOW`, Windows buffer overflow) and inotify isn't
     recursive, so real-time must supplement — not replace — a periodic full
     scan that catches anything missed.
   - *Cadence:* each debounced flush is a snapshot (cheap — blobs are shared
     and only the manifest + changed blobs are new); rely on retention to
     prune, and likely collapse the run history so live changes don't flood
     it. Consider `realtime_debounce` and a min snapshot interval.
4. **Schedule catch-up across server downtime** — today catch-up covers a
   *client* being offline while the server is up (a missed occurrence runs on
   reconnect). If the *server* is down at a scheduled time, that occurrence
   isn't retroactively detected on restart. Persist each job's last scheduled
   run and, on startup, fire catch-ups for anything overdue.
5. **Client-side encryption** — per-client or per-job keys, encrypt blobs
   and manifests before upload so the server never sees plaintext.
6. **Windows VSS** — snapshot volumes before reading so locked/open files
   (databases, mailboxes) are captured consistently instead of skipped.
7. **Bandwidth limits & windows** — per-agent upload throttling and
   allowed backup windows.
8. **Notifications** — email/webhook on failed or missed runs.
9. **Chunk-based deduplication** — content-defined chunking for large
   frequently-modified files (VM images, mailbox files) so only changed
   chunks upload; the current file-level model stays for everything else.
10. **Multi-admin / roles** and audit log.
