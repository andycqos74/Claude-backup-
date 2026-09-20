# Central storage-OAuth callback

One redirect URI, shared by every tenant, so connecting OneDrive (or Google
Drive) is three clicks for the customer instead of a trip through the Azure
portal — and so tenant count is not capped by the provider.

## Why

Two problems with a callback per tenant subdomain:

1. **The customer has to create their own app registration.** Today the
   admin pastes a client ID and secret they obtained themselves. That is six
   steps in a Microsoft admin portal before they can even click Connect, and
   it is the least simple thing in the product.
2. **Providers cap redirect URIs per application.** Microsoft allows at most
   **100** for apps that support personal accounts and **250** for work
   accounts, and [the limit cannot be
   raised](https://learn.microsoft.com/en-us/entra/identity-platform/reply-url).
   One callback per tenant subdomain is therefore a hard ceiling of 100
   tenants on consumer OneDrive.

The documented way around both is to register **one** redirect URI and carry
the destination in the OAuth `state` parameter. That is what this implements.

```
 1. admin clicks Connect on acme.gui.example.com
 2. browser → Microsoft, redirect_uri=https://connect.example.com/oauth/callback
                         state=acme.<nonce>
 3. Microsoft → https://connect.example.com/oauth/callback?code=…&state=acme.<nonce>
 4. oauth-forwarder reads "acme", validates it, 302s the browser to
    https://acme.gui.example.com/api/admin/storage/oauth/callback?code=…&state=…
 5. the acme container exchanges the code itself, exactly as a single-tenant
    deployment does
```

Nothing secret passes through the forwarder: no client secret, no token. The
authorization code does, in a query string, so the forwarder never logs the
query.

## What the customer does

1. **Settings → Backup storage → OneDrive**
2. Optionally name a folder
3. **Connect OneDrive**
4. Sign in with their Microsoft account
5. **Accept** the consent screen
6. Back on Settings: *Connected — them@example.com*

No portal, no client ID, no secret.

## Configuration

### The operator's app registration (once)

- Supported account types: **work/school and personal Microsoft accounts**
  (this matches the `/common` authority the server already uses).
- Redirect URI (Web): `https://connect.example.com/oauth/callback` — just
  the one.
- A client secret.
- Worth doing: **publisher verification**, so the consent screen shows a
  verified publisher rather than an unverified-app warning.

### Each tenant server

| Variable | Example | Meaning |
|---|---|---|
| `CB_OAUTH_CALLBACK_URL` | `https://connect.example.com/oauth/callback` | The shared redirect URI. Must match the registered value byte for byte |
| `CB_TENANT_SLUG` | `acme` | Identifies this tenant in the OAuth state. Must be hostname-safe |

The provisioner also seeds the operator's client ID and secret into the
tenant's storage settings, so the GUI can hide the app-credentials form and
show only the Connect button.

Both are optional. With neither set, the server behaves exactly as before:
its own callback on its own origin, single-tenant.

### The forwarder

| Variable | Default | Meaning |
|---|---|---|
| `CB_TENANT_TEMPLATE` | – (required) | Where tenants live, e.g. `https://{slug}.gui.example.com` |
| `CB_TENANT_ALLOWLIST` | – | Comma-separated slugs. **Recommended** |
| `CB_CALLBACK_PATH` | `/oauth/callback` | Path this listens on |
| `CB_TENANT_PATH` | `/api/admin/storage/oauth/callback` | Path on the tenant |
| `CB_LISTEN` | `:8080` | Plain HTTP; put it behind the same TLS terminator as the GUIs |

Route `connect.example.com` to it exactly as you route a tenant GUI — for
cloudflared, one more public hostname pointing at `oauth-forwarder:8080`.

## Two things that will bite

**The redirect URI must be identical in three places.** The authorization
request, the token exchange, and every later background refresh all send
`redirect_uri`, and providers require them to match. They all read it from
the same config, and `TestCentralCallbackEndToEnd` asserts the value that
actually reaches the token endpoint — because a mismatch surfaces as a bare
`invalid_grant` with no hint as to why.

**The slug is substituted into a hostname**, so it is validated strictly:
lower-case alphanumerics and inner hyphens, nothing else. A state that is not
exactly `<valid-slug>.<dotless-nonce>` is refused rather than partially
interpreted — `evil.com.nonce` names no tenant rather than naming `evil`.
Setting `CB_TENANT_ALLOWLIST` narrows it further to slugs you actually run.

The nonce is unchanged and still compared in full by the tenant server, so
the CSRF protection of the original design is intact — the forwarder only
reads the routing prefix.

## Business customers may hit "Need admin approval"

Microsoft Entra lets an organisation restrict user consent to third-party
applications. A customer on a locked-down Microsoft 365 tenant will see a
request-approval wall instead of a consent screen, and needs their own IT
admin to grant consent. That is a provider-side policy, not something this
can work around; it is worth surfacing the error clearly rather than letting
it look like a bug in the backup product.

## Scope note

The server currently requests `Files.ReadWrite`, which the consent screen
presents as full access to the customer's OneDrive. `Files.ReadWrite.AppFolder`
limits it to an app-specific folder and reads far better on the consent
screen. Worth evaluating against how blobs are laid out before switching.
