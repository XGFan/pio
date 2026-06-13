# PIO — Proxies In One

A self-hosted forward-proxy manager. It keeps a pool of upstream proxies —
synced from [Webshare](https://www.webshare.io/) API keys and/or added
manually — and exposes them locally through a single port that speaks **both
HTTP and SOCKS5**. Clients authenticate with credentials the daemon owns; the
daemon rewrites auth and tunnels each connection to the chosen upstream.

It ships as a Go daemon (`piod`) with two admin surfaces: a macOS
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
    Only upstreams with an **unambiguous** display name are routable this way.
- **Upstream sources:** Webshare API keys (periodic sync of the proxy list)
  and manually-added HTTP / HTTPS / SOCKS5 proxies.
- **Built-in `default` upstream.** A reserved, always-present upstream named
  `default` that egresses straight out of the daemon's own host network — no
  upstream hop. Map a user to it (it's offered in the admin UI's mapping
  dropdown), or reach it by name (`username = default`) via the universal
  password / subscription. See [Built-in `default` upstream](#built-in-default-upstream).
- **Subscription endpoint.** When enabled (and a universal password is set),
  the daemon serves a public `GET /subscription?password=…` that returns a
  subscription list for proxy clients — one line per routable proxy. A `type`
  query parameter picks the line scheme: `socks`/`socks5`/omitted → SOCKS,
  `type=http` → HTTP-proxy lines.
- **Chrome extension.** A Manifest V3 browser extension ([`extension/`](extension/))
  that consumes a subscription URL, lists the proxies, and applies a chosen one
  browser-wide — authenticating it automatically via
  `chrome.webRequest.onAuthRequired`. See [`extension/README.md`](extension/README.md).
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

- `cmd/piod` — the daemon entry point.
- `internal/listener` — the unified HTTP/SOCKS5 listener and per-protocol handlers.
- `internal/tunnel` — credential resolution (`Acquire`) and upstream dialing.
- `internal/routing` — the immutable in-memory routing snapshot (COW/RCU swap).
- `internal/repo` / `internal/store` — SQLite access and embedded migrations.
- `internal/api` — the JSON REST surface (loopback, used by the macOS app).
- `internal/web` — the LAN web admin panel (cookie auth) + public `/subscription`.
- `ui/PIO` — the macOS SwiftUI menu-bar app.
- `extension/` — the Chrome (MV3) browser proxy-switcher extension.

## Running

Build and run the daemon:

```sh
go build -o piod ./cmd/piod

# Loopback-only (macOS app talks to the unauthenticated loopback API):
./piod run --data-dir ./data

# Also expose the LAN web admin panel:
PIO_WEB_PASSWORD=secret ./piod run \
  --data-dir ./data --web-bind 0.0.0.0:9090
```

### CLI

```
piod version
piod add-key --label=<s> --key=<sk_...> [--data-dir=<path>]
piod sync    --key-id=<id>              [--data-dir=<path>]
piod run     [--data-dir=<path>] [--web-bind=<addr>] [--web-password=<s>]
             [--web-auth-mode=password|forward-auth] [--web-auth-header=<h>]
```

- `--web-bind` — serve the web panel on this address (disabled when empty).
- `--web-auth-mode` — how the panel authenticates (default `password`):
  - `password` — the panel's own cookie-session password challenge.
  - `forward-auth` — trust an identity header injected by an upstream
    forward-auth proxy (e.g. tinyauth). No password challenge; the panel is
    open to anyone who can reach it directly, so **it MUST sit behind a proxy
    that authenticates requests and strips client-supplied copies of the
    header**.
- `--web-password` — required in `password` mode; prefer the
  `$PIO_WEB_PASSWORD` env var to keep it out of the process list. Ignored in
  `forward-auth` mode.
- `--web-auth-header` — in `forward-auth` mode, the request header carrying the
  authenticated identity (default `Remote-Email`; the panel only checks that it
  is present and non-empty).

### Environment overrides (for declarative deploys)

| Variable | Effect |
| --- | --- |
| `PIO_WEB_PASSWORD` | Web admin panel password (alternative to `--web-password`; used in `password` mode). |
| `PIO_WEB_AUTH_MODE` | `password` (default) or `forward-auth` (alternative to `--web-auth-mode`). |
| `PIO_WEB_AUTH_HEADER` | Forward-auth identity header (alternative to `--web-auth-header`; default `Remote-Email`). |
| `PIO_PROXY_BIND` | Force the proxy listener bind address (e.g. `0.0.0.0`); persisted back to the DB on boot. |
| `PIO_PROXY_AUTOSTART` | `true`/`1` starts the proxy listener on boot. |

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

## Built-in `default` upstream

The daemon ships with one reserved upstream, **`default`**, seeded automatically
on boot. Routing to it dials the target **straight out of the daemon's own host
network** — there is no upstream proxy hop, so traffic exits from the machine
the daemon runs on.

> **Upgrade note (breaking).** This upstream was historically named `direct`;
> migration 0011 renames it to `default` and repoints any per-user mappings
> automatically. Subscription-driven clients self-heal (the list now emits the
> `default` line), but any client **hand-configured with `username=direct`** via
> the universal password must be updated to `username=default`.

- **How to use it.** Either map a local user to it — it appears in the admin
  UI's user→upstream mapping dropdown alongside synced/manual proxies — or, with
  a universal password set, connect with `username = default` and
  `password = <universal-password>`. It is also emitted in the subscription list
  as `socks://default:…@host:port#default`.
- **Built-in, not a managed proxy.** `default` is selectable as a mapping target
  but is otherwise immutable: it can't be edited, renamed, replaced, or deleted,
  it never appears under an API key's upstream list or the manual-proxy list, and
  a Webshare sync never removes it. It is addressed purely by name.
- **⚠️ Security — wider egress reach.** Unlike a remote upstream (which exits
  from the proxy's network), `default` exits from the daemon's host, so any
  client routed to it can reach that host's local and internal network —
  including `localhost`, RFC1918 services, and cloud metadata
  (`169.254.169.254`). Because it is reachable by anyone holding the universal
  proxy password, **distribute that password accordingly** (or rely on per-user
  mapping only).

## Subscription

When **subscription is enabled** and a **universal password is set**, the web
server exposes:

```
GET /subscription?password=<universal-password>&type=<socks|socks5|http>
```

- Authentication is the `password` query parameter only — **no cookie**.
  Wrong/missing password → `401`; the endpoint returns `404` when disabled or
  no universal password is set. Failed attempts are rate-limited per IP by the
  shared deny-list (10 failures / 60s → 5-minute ban).
- The optional `type` parameter selects the line scheme. `socks`, `socks5`, and
  the omitted default all emit SOCKS lines; `http` emits HTTP-proxy lines. Both
  point at the **same** unified proxy port (it auto-detects the protocol from
  the first byte per connection) — only the URI scheme differs.
- Response is `text/plain`, one line per routable proxy:

  ```
  # type=socks | type=socks5 | (omitted)
  socks://{display-name}:{universal-password}@{subscription-host}:{mixed-port}#{display-name}

  # type=http
  http://{display-name}:{universal-password}@{subscription-host}:{mixed-port}#{display-name}
  ```

The web panel's **Subscription** card has a "Copy subscription URL" button
that yields the full URL (including `?password=`), built from the panel's own
origin (where `/subscription` is served).

The `type=http` form exists for clients that can only use authenticated HTTP
proxies — notably the **Chrome extension** under [`extension/`](extension/),
which applies a chosen proxy browser-wide and supplies the per-proxy
credentials via `chrome.webRequest.onAuthRequired` (Chrome cannot authenticate
SOCKS proxies). See [`extension/README.md`](extension/README.md).

## Deployment

`Dockerfile` builds a static (CGO-free) image; `deploy/k8s.yaml` runs it with
a PVC for `/data`, the web panel on `:9090` behind a Traefik ingress, and the
unified proxy port (`:8080`) exposed via a MetalLB `LoadBalancer` Service.
CI (`.woodpecker.yaml`) applies the manifest and rolls the new image on each
push to `master`.

The shipped manifest runs the panel in **`forward-auth` mode** behind the
cluster's tinyauth (a Traefik `forwardAuth` middleware): users authenticate at
tinyauth once and PIO trusts the `Remote-Email` it injects, so there is no
second password prompt. The panel is gated by a single port-only-via-Traefik
guarantee — the `pio` Service is `ClusterIP` (reachable solely through the
ingress) and only the proxy port `:8080` is exposed to the LAN. A separate
`pio-public` Ingress serves **`/subscription` without tinyauth** so machine
clients (the Chrome extension, proxy auto-config) keep working off the
`?password=` query auth. To run without tinyauth, drop `PIO_WEB_AUTH_MODE`
(falling back to the `PIO_WEB_PASSWORD` challenge) and remove
`default-tinyauth@kubernetescrd` from the `pio` Ingress.

## Development

```sh
go build ./...
go test ./...
( cd ui/PIO && swift build )   # macOS app
```

Migrations are plain `.sql` files under `internal/store/migrations/`, applied
in lexicographic order and recorded in `schema_migrations`.
