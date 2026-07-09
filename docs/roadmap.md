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
2. **Client-side encryption** — per-client or per-job keys, encrypt blobs
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
