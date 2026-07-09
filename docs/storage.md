# Backup storage backends

By default the server stores all backup data (content-addressed blobs and
snapshot manifests) on its own local disk — the `/data` volume in Docker,
or `CB_DATA_DIR` for a systemd install. You can instead point it at cloud
storage in your own account.

Configure this in the GUI: **Settings → Backup storage backend**.

## Supported backends

| Backend | Status | Notes |
|---|---|---|
| Local disk | ✅ default | Backups stored on the server host. |
| OneDrive / OneDrive for Business | ✅ | Microsoft Graph; personal or M365. |
| Google Drive | ✅ | `drive.file` scope (app sees only its own files). |
| Box | 🚧 coming soon | Needs Box's chunked-upload protocol for large objects. |

## How connecting works

Because this is self-hosted software, you register your own OAuth app with
the provider once (there is no shared app). The flow:

1. Register an app in the provider's developer console and add the server's
   **redirect URL** (shown on the Settings page) to it. Step-by-step guides:
   - OneDrive: `/static/storage-onedrive.html`
   - Google Drive: `/static/storage-gdrive.html`
2. Paste the app's **Client ID** and **Client secret** into Settings, pick a
   destination folder (default `central-backup`), and **Save**.
3. Click **Connect & authorise**, sign in to the provider, and approve. The
   server stores a refresh token and switches new backups to that account.

Credentials are stored in the server database (unencrypted at rest, like
the rest of the system's data — treat the server host as sensitive). The
client secret and refresh token are never sent back to the browser.

## Important behaviours

- **Switching backends does not move existing data.** Snapshots already
  written to the previous backend stay there and are restorable only while
  that backend is active. Plan a switch when you're starting a fresh backup
  set, or keep the old backend if you still need those snapshots. (Automatic
  migration is on the roadmap.)
- **One active backend at a time.** There is no simultaneous local+cloud
  mirror yet (also on the roadmap). If you want 3-2-1 today, run periodic
  syncs of the local `/data/storage` directory with your own tooling.
- **Fallback on startup.** If a configured cloud backend can't be built at
  startup (e.g. an expired/revoked token or a lapsed client secret), the
  server logs the problem and falls back to local storage so backups keep
  working — reconnect in Settings to restore cloud storage.
- **Token/secret expiry.** Provider client secrets expire (you choose the
  lifetime when creating them). Before expiry, create a new secret in the
  provider console, re-save it in Settings, and reconnect.

## Bandwidth and performance

Cloud uploads go **server → provider**, so the server's uplink is the
bottleneck, not the clients'. Incremental backups still only upload changed,
deduplicated, compressed blobs, so ongoing volume is small; the first full
backup of a large data set is the heavy one.
