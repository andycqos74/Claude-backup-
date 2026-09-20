# Putting the GUI behind SSL with Cloudflare Tunnel

Goal: open the web GUI at `https://gui.example.com` with a real,
browser-trusted certificate, no self-signed warning, and no inbound port
for it — while backups keep working.

The catch is that this server serves two very different audiences on one
port, and only one of them can go through a tunnel:

| Traffic | Goes through the tunnel? | Why |
|---|---|---|
| Operator's browser (GUI, admin API) | **Yes** | It only needs *some* valid certificate; Cloudflare's is ideal. |
| Agents (enroll, blobs, control channel) | **No** | Agents pin *this server's* certificate fingerprint. Cloudflare terminates TLS with its own certificate, so a tunnelled agent can never verify the server. |

So the server can run a second, **GUI-only listener** for the tunnel, while
agents keep talking to port 8443 directly. That split is what makes this
safe: `CB_GUI_LISTEN` serves the GUI, the admin API, `/static` and `/dl` —
and *not* `/api/agent/*`. Enrollment and blob storage are never exposed
through the tunnel, even by accident.

```
        browser ──https──▶ Cloudflare edge ──tunnel──▶ cloudflared ──http──▶ :8080  (GUI only)
                                                                              │
                                                            same server ──────┤
                                                                              │
        agents  ─────────────https, cert pinned, direct───────────────────▶ :8443  (everything)
```

## What you need

- A domain on Cloudflare (any plan, including free).
- The server still reachable by agents on **TCP 8443** — the tunnel does
  not remove that requirement, it only covers the GUI. If inbound 8443 is
  impossible for you, see
  [no inbound port at all](#if-you-cannot-open-8443-at-all) below.

## Settings

| Variable | Example | What it does |
|---|---|---|
| `CB_GUI_LISTEN` | `:8080` (Docker) or `127.0.0.1:8080` (systemd) | Plain-HTTP GUI-only listener for the tunnel. **Never publish this port.** |
| `CB_GUI_URL` | `https://gui.example.com` | The address your browser uses. Baked into the storage OAuth redirect URL. |
| `CB_PUBLIC_URL` | `https://backup.example.com:8443` | The **direct** address agents use. Required once `CB_GUI_LISTEN` is set. |
| `CB_SERVER_NAME` | `backup.example.com` | Hostname/IP in the server's own certificate (what agents pin). |

`CB_PUBLIC_URL` and `CB_GUI_URL` are deliberately separate, and usually
different hostnames. Enrollment commands, downloaded installers and the
certificate fingerprint all come from `CB_PUBLIC_URL`, so an installer
downloaded *through the tunnel* still points agents at the direct address.

The server refuses to start if `CB_GUI_LISTEN` is set without
`CB_PUBLIC_URL`, rather than quietly minting enrollment commands that point
at the tunnel.

## Docker Compose

```bash
CB_SERVER_NAME=backup.example.com \
CB_PUBLIC_URL=https://backup.example.com:8443 \
CB_GUI_URL=https://gui.example.com \
CB_TUNNEL_TOKEN=<token from the Cloudflare dashboard> \
  docker compose -f deploy/docker-compose.yml \
                 -f deploy/docker-compose.cloudflared.yml up -d
```

The overlay adds a `cloudflared` container and sets `CB_GUI_LISTEN=:8080`
on the server. Port 8080 is **not** published — only cloudflared, on the
same Docker network, can reach it.

Then, in **Zero Trust → Networks → Tunnels → your tunnel → Public
hostname**, add:

- **Subdomain/domain**: `gui.example.com`
- **Service type**: `HTTP`
- **URL**: `backup-server:8080`

HTTP, not HTTPS: the hop from cloudflared to the server stays inside the
Docker network, and the GUI listener is plain HTTP on purpose. (Pointing it
at `https://backup-server:8443` also "works" only with TLS verification
disabled, and would expose the agent API through the tunnel — don't.)

## Portainer

Portainer can't use compose overlays, so paste
`deploy/portainer-stack-cloudflared.yml` in its place: it is the normal
Portainer stack with the `cloudflared` connector and the two GUI settings
already filled in. **Stacks → your stack → Editor**, replace the contents,
set `CB_GUI_URL` and `CB_TUNNEL_TOKEN` under *Environment variables*
alongside the ones you already have, and **Update the stack** with
*Re-pull image* enabled.

Check `docker volume ls | grep backup` first and make the two `name:`
fields match your existing volumes — the file ships with the
`deploy_`-prefixed defaults.

Then add the public hostname in the Cloudflare dashboard exactly as in the
Docker section above (`HTTP` → `backup-server:8080`).

## systemd (no Docker)

Install the server with the GUI listener on loopback:

```bash
sudo CB_SERVER_NAME=backup.example.com \
     CB_PUBLIC_URL=https://backup.example.com:8443 \
     CB_GUI_LISTEN=127.0.0.1:8080 \
     CB_GUI_URL=https://gui.example.com \
     scripts/install-server.sh
```

Then create a locally managed tunnel:

```bash
cloudflared tunnel login
cloudflared tunnel create backup-gui
cloudflared tunnel route dns backup-gui gui.example.com
```

Copy `deploy/cloudflared-config.example.yml` to `/etc/cloudflared/config.yml`,
fill in the tunnel UUID, credentials path and hostname, then:

```bash
cloudflared tunnel ingress validate
sudo cloudflared service install
```

Its ingress rules also refuse `/api/agent/*` outright — redundant with the
listener split, but it means a future misconfiguration can't publish
enrollment to the internet.

## Lock the GUI down with Cloudflare Access

A tunnel makes the GUI reachable from anywhere, which the login page is now
the only thing standing in front of. Put **Cloudflare Access** in front of
it (Zero Trust → Access → Applications → Add a self-hosted application for
`gui.example.com`) so visitors must pass your identity policy — an email
one-time PIN, Google/Microsoft SSO, whatever you use — before the request
ever reaches the server. That is the main security benefit of this setup
over opening a port, and it costs nothing on the free tier.

The server's own login still applies underneath.

## One limit to know about: 100 MB uploads

Cloudflare caps proxied **request bodies** at 100 MB on the Free and Pro
plans. Everything in the GUI is small except one thing: *pushing a file to
a client* (Clients → a client → Send file), which the server itself allows
up to 2 GiB. A larger push through the tunnel fails at the Cloudflare edge
with **error 413**, before it reaches the server.

Downloads are unaffected — restore downloads and snapshot zips stream out
as responses, at any size.

If you need to push something bigger, open the GUI on the direct address
(`https://backup.example.com:8443`, self-signed warning and all) for that
one operation. Both listeners serve the same GUI and the same session.

## Verify

1. **GUI**: open `https://gui.example.com` — a valid certificate, no
   warning, and the login page.
2. **Agent API is not exposed**:
   ```bash
   curl -s -o /dev/null -w '%{http_code}\n' https://gui.example.com/api/agent/enroll
   # 404 — correct. Anything else means the tunnel is pointed at :8443.
   ```
3. **Agents still verify directly**:
   ```bash
   openssl s_client -connect backup.example.com:8443 </dev/null 2>/dev/null \
     | openssl x509 -noout -subject -fingerprint -sha256
   ```
   The subject should be `CN=central-backup-server` (or your own cert) and
   the fingerprint must equal the one in the server log — *not* a
   Cloudflare certificate.
4. **Enrollment points at the direct address**: in the GUI (reached through
   the tunnel), open **Clients → Enroll new client**. The command shown
   must contain `https://backup.example.com:8443`, not `gui.example.com`.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Login form accepts the password but bounces back to the login page | The GUI is being reached over plain `http://`. The session cookie is `Secure`-only, so the browser drops it. Reach the GUI through the tunnel (https), not by the GUI port. |
| `502 Bad Gateway` from Cloudflare | cloudflared can't reach the origin. Check the public-hostname URL is `http://backup-server:8080` (Docker) or `http://127.0.0.1:8080` (systemd), and that `CB_GUI_LISTEN` is set. |
| `Client sent an HTTP request to an HTTPS server` | The tunnel is pointed at port 8443 with an `HTTP` service type. Point it at the GUI listener instead. |
| Agents log `server certificate fingerprint mismatch` | Agents are going through the tunnel. Their `server_url` must be the direct `https://host:8443`; fix `CB_PUBLIC_URL` and re-issue the enrollment, or patch `creds.json` as in [deployment.md](deployment.md#removing-a-tunnel-from-the-agent-address). |
| OAuth storage connect fails with a redirect-URI mismatch | `CB_GUI_URL` is unset or differs from the address you browse. Set it, then register `https://gui.example.com/api/admin/storage/oauth/callback` with the provider. |
| GUI works, enrollment command shows the tunnel hostname | `CB_PUBLIC_URL` is wrong — it must be the direct agent address. |
| Cloudflare **error 413** when sending a file to a client | The file is over Cloudflare's 100 MB proxied-upload cap. Use the direct address for that push (see above). |

## If you cannot open 8443 at all

A tunnel is not a workaround for agent traffic: pinning fails regardless.
The options are

- a **VPN/WireGuard** link, with agents connecting to the server's private
  address (`CB_PUBLIC_URL=https://10.x.x.x:8443`), or
- a **TCP passthrough** proxy — nginx `stream` with `proxy_pass` and no
  `ssl_certificate` — which forwards bytes without terminating TLS, so the
  pin survives, or
- **Cloudflare Tunnel + `cloudflared access tcp`** on every client, which
  tunnels the raw TCP port rather than HTTP and so also preserves the pin.
  This needs `cloudflared` installed and running on each client, which is
  usually more moving parts than the agent itself.

The GUI-behind-a-tunnel setup on this page is independent of all three and
can be combined with any of them.
