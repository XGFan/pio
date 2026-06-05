# webshare-proxy

A self-hosted forward-proxy manager. It keeps a pool of upstream proxies —
synced from [Webshare](https://www.webshare.io/) API keys and/or added
manually — and exposes them locally through a single port that speaks **both
HTTP and SOCKS5**. Clients authenticate with credentials the daemon owns; the
daemon rewrites auth and tunnels each connection to the chosen upstream.

It ships as a Go daemon (`webshare-proxyd`) with two admin surfaces: a macOS
menu-bar app and an optional cookie-protected LAN web panel.

## Features

- **Unified single proxy port.** One TCP port serves HTTP and SOCKS5; the
  protocol is auto-detected per connection from the first byte (`0x05` →
  SOCKS5, otherwise HTTP). Default `127.0.0.1:8080`, bindable to `0.0.0.0`
  for LAN/remote exposure. UDP ASSOCIATE / BIND are rejected — upstreams are
  TCP-CONNECT only.
- **Two ways to authenticate / route:**
  - **Per-user mapping** — each local user (username + password) is mapped to
    one upstream. The client uses those credentials; the daemon routes to the
    mapped proxy.
  - **Universal password + display name** — set one daemon-wide *universal
    proxy password*; any client can then connect with
    `username = a proxy's display name` and `password = the universal
    password` to route through that specific proxy, no per-proxy user needed.
    Only **alive** upstreams with an **unambiguous** display name are routable
    this way.
- **Upstream sources:** Webshare API keys (periodic sync of the proxy list)
  and manually-added HTTP / HTTPS / SOCKS5 proxies.
- **Subscription endpoint.** When enabled (and a universal password is set),
  the daemon serves a public `GET /subscription?password=…` that returns a
  SOCKS subscription list for proxy clients — one line per routable proxy.
- **Hot-switch routing.** Remapping a user or editing/deleting an upstream
  tears down in-flight connections to the old target within ~1 TCP RTT.
- **Encrypted at rest.** API keys, upstream passwords, and the universal
  password are AES-256-GCM encrypted with a per-install master key. (Local
  user passwords are stored plaintext by design, so the UI can reveal them;
  see the threat model note below.)

## Architecture

```
                    ┌──────────────────────────────────────────┐
  proxy clients ──▶ │ Unified listener  (HTTP + SOCKS5, 1 port) │
                    │   └─ sniff first byte → dispatch           │
                    └───────────────┬───────────────────────────┘
                                    │ Acquire(user, pass)
                    ┌───────────────▼───────────────┐
                    │ routing.Core (in-memory, RCU)  │ ◀── SQLite (data.db)
                    │   Users / Upstreams /          │     migrations, settings,
                    │   ByDisplayName / UniversalPwd │     api_keys, local_users
                    └───────────────┬───────────────┘
                                    │ DialUpstream (http/https/socks5 CONNECT)
                                    ▼
                              upstream proxy ──▶ target

  admin: macOS app + web panel ─▶ REST API (loopback) / web panel (LAN)
  public: GET /subscription (query-param auth only)
```

- `cmd/webshare-proxyd` — the daemon entry point.
- `internal/listener` — the unified HTTP/SOCKS5 listener and per-protocol handlers.
- `internal/tunnel` — credential resolution (`Acquire`) and upstream dialing.
- `internal/routing` — the immutable in-memory routing snapshot (COW/RCU swap).
- `internal/repo` / `internal/store` — SQLite access and embedded migrations.
- `internal/api` — the JSON REST surface (loopback, used by the macOS app).
- `internal/web` — the LAN web admin panel (cookie auth) + public `/subscription`.
- `ui/WebshareProxy` — the macOS SwiftUI menu-bar app.

## Running

Build and run the daemon:

```sh
go build -o webshare-proxyd ./cmd/webshare-proxyd

# Loopback-only (macOS app talks to the unauthenticated loopback API):
./webshare-proxyd run --data-dir ./data

# Also expose the LAN web admin panel:
WEBSHARE_WEB_PASSWORD=secret ./webshare-proxyd run \
  --data-dir ./data --web-bind 0.0.0.0:9090
```

### CLI

```
webshare-proxyd version
webshare-proxyd add-key --label=<s> --key=<sk_...> [--data-dir=<path>]
webshare-proxyd sync    --key-id=<id>              [--data-dir=<path>]
webshare-proxyd run     [--data-dir=<path>] [--web-bind=<addr>] [--web-password=<s>]
```

- `--web-bind` — serve the web panel on this address (disabled when empty).
- `--web-password` — required when `--web-bind` is set; prefer the
  `$WEBSHARE_WEB_PASSWORD` env var to keep it out of the process list.

### Environment overrides (for declarative deploys)

| Variable | Effect |
| --- | --- |
| `WEBSHARE_WEB_PASSWORD` | Web admin panel password (alternative to `--web-password`). |
| `WEBSHARE_PROXY_BIND` | Force the proxy listener bind address (e.g. `0.0.0.0`); persisted back to the DB on boot. |
| `WEBSHARE_PROXY_AUTOSTART` | `true`/`1` starts the proxy listener on boot. |

### Data directory & secrets

State lives under the data dir (`<data-dir>/data.db` plus
`<data-dir>/master.key`, mode `0600`). The master key decrypts every other
secret; back it up alongside the database, and treat read access to both as
equivalent to full compromise.

## Settings

Edited in the admin UI (or via `PUT /api/v1/settings`):

| Setting | Meaning |
| --- | --- |
| Listen addr (`proxy_bind`) | Interface the unified proxy binds to. |
| Mixed Port (`proxy_port`) | The single HTTP+SOCKS5 proxy port (default 8080). |
| Sync interval (`sync_interval_minutes`) | Webshare resync cadence. |
| Universal password | Master credential for display-name routing (set via `PUT /api/v1/settings/universal-password`; never returned by GET). |
| Subscription enabled / host | Gate + public host for the subscription endpoint. |

Proxy on/off is controlled separately via `POST /api/v1/proxy/start` /
`/stop` so the listener state machine stays authoritative.

## Subscription

When **subscription is enabled** and a **universal password is set**, the web
server exposes:

```
GET /subscription?password=<universal-password>
```

- Authentication is the `password` query parameter only — **no cookie**.
  Wrong/missing password → `401`; the endpoint returns `404` when disabled or
  no universal password is set. Failed attempts are rate-limited per IP by the
  shared deny-list (10 failures / 60s → 5-minute ban).
- Response is `text/plain`, one line per routable proxy:

  ```
  socks://{display-name}:{universal-password}@{subscription-host}:{mixed-port}#{display-name}
  ```

The web panel's **Subscription** card has a "Copy subscription URL" button
that yields the full URL (including `?password=`), built from the panel's own
origin (where `/subscription` is served).

## Deployment

`Dockerfile` builds a static (CGO-free) image; `deploy/k8s.yaml` runs it with
a PVC for `/data`, the web panel on `:9090` behind a Traefik ingress, and the
unified proxy port (`:8080`) exposed via a MetalLB `LoadBalancer` Service.
CI (`.woodpecker.yaml`) applies the manifest and rolls the new image on each
push to `master`.

## Development

```sh
go build ./...
go test ./...
( cd ui/WebshareProxy && swift build )   # macOS app
```

Migrations are plain `.sql` files under `internal/store/migrations/`, applied
in lexicographic order and recorded in `schema_migrations`.
