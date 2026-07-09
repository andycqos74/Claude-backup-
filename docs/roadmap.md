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
3. **Client-side encryption** — per-client or per-job keys, encrypt blobs
   and manifests before upload so the server never sees plaintext.
3. **Windows VSS** — snapshot volumes before reading so locked/open files
   (databases, mailboxes) are captured consistently instead of skipped.
4. **Bandwidth limits & windows** — per-agent upload throttling and
   allowed backup windows.
5. **Notifications** — email/webhook on failed or missed runs.
6. **Chunk-based deduplication** — content-defined chunking for large
   frequently-modified files (VM images, mailbox files) so only changed
   chunks upload; the current file-level model stays for everything else.
7. **Multi-admin / roles** and audit log.
