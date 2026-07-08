# Roadmap

Planned enhancements, roughly in priority order.

1. **Cloud storage backends** — Google Drive and OneDrive implementations
   of the `storage.Backend` interface (OAuth device flow, chunked upload,
   rate limiting), plus mirroring local storage to a secondary backend for
   3-2-1 style off-site copies. The interface is already in place
   (`internal/server/storage`).
2. **Client-side encryption** — per-client or per-job keys, encrypt blobs
   and manifests before upload so the server never sees plaintext.
3. **Windows VSS** — snapshot volumes before reading so locked/open files
   (databases, mailboxes) are captured consistently instead of skipped.
4. **Bandwidth limits & windows** — per-agent upload throttling and
   allowed backup windows.
5. **Notifications** — email/webhook on failed or missed runs.
6. **Run cancellation** and richer live progress in the GUI.
7. **Chunk-based deduplication** — content-defined chunking for large
   frequently-modified files (VM images, mailbox files) so only changed
   chunks upload; the current file-level model stays for everything else.
8. **Multi-admin / roles** and audit log.
