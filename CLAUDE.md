# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

PIO ("Proxies In One") is a self-hosted forward-proxy manager: a Go daemon (`piod`)
that pools upstream proxies (synced from Webshare API keys and/or added manually)
and exposes them locally through **one port that speaks both HTTP and SOCKS5**.
Admin surfaces are a macOS SwiftUI menu-bar app (loopback REST) and an optional
LAN web panel (default: password cookie session; or `forward-auth` mode that
trusts an upstream proxy's identity header, e.g. behind tinyauth). README.md is
the authoritative feature/spec doc —
read it for behavior questions; this file covers commands and the architecture that
spans multiple files.

## Commands

```sh
go build ./...                       # build all Go packages
go build -o piod ./cmd/piod          # build the daemon binary
go vet ./...                         # what CI runs first
go test ./...                        # full suite (unit + test/integration)
go test ./internal/routing/          # one package
go test ./internal/tunnel/ -run TestAcquire   # one test by name
go test -tags=lockaudit ./...        # run the lock-order assertion harness (see below)
( cd ui/PIO && swift build )         # macOS SwiftUI app
./scripts/build-app.sh [out-dir]     # build daemon + app, package PIO.app (macOS only, default ./dist)
./scripts/build-extension.sh [out-dir]   # zip the Chrome extension → dist/pio-extension-<version>.zip
```

CI (`.woodpecker.yaml`, on push to `master`): `go vet` → `go test -timeout 3m` →
docker buildx → `kubectl apply deploy/k8s.yaml` + image roll. There is no separate
lint step; `go vet` is the gate.

Run locally:
```sh
./piod run --data-dir ./data                                   # loopback-only (Mac app talks to it)
PIO_WEB_PASSWORD=secret ./piod run --data-dir ./data --web-bind 0.0.0.0:9090   # + LAN web panel
```
Subcommands: `version`, `add-key --label --key`, `sync --key-id`, `run`. Env overrides
for declarative deploys: `PIO_WEB_PASSWORD`, `PIO_PROXY_BIND`, `PIO_PROXY_AUTOSTART`
(applied in `run.go`, persisted back to the DB on boot).

## Architecture

Data plane (per proxy connection) and control plane (admin REST/web) share one
in-memory routing snapshot; SQLite is the durable backing store, read only at boot
and on explicit rebuilds — **never on the hot path**.

```
client ─▶ internal/listener (unified HTTP+SOCKS5, 1 port, sniff first byte)
            └─▶ tunnel.Acquire(user,pass) ─▶ routing.Core snapshot (in-memory, RCU)
                  └─▶ tunnel.DialUpstream ─▶ upstream proxy (or "default") ─▶ target
admin: macOS app + web panel ─▶ internal/api (chi router, /api/v1/*) ─▶ routing.Core + repo
```

Package map (`internal/`):
- `cli` — subcommand dispatch; `run.go` is the daemon wiring (owns the goroutine
  lifecycle, env overrides, sync ticker, signal handling, and wires all the
  `api.Deps` callbacks that close over `routing.Core` + `registry`).
- `listener` — `unified.go` accepts a conn, reads ONE byte (`0x05` → SOCKS5, else
  HTTP), replays it via `prefixConn`, dispatches to `socks5.go`/`http_proxy.go`
  per-connection handlers. UDP ASSOCIATE / BIND are rejected (TCP CONNECT only).
- `tunnel` — `Acquire` resolves creds against the snapshot; `DialUpstream` dispatches
  the built-in `default` upstream (on Source) to a straight host-network dial, else
  on protocol (http/https-CONNECT, socks5); `Bridge` does the duplex copy.
- `routing` — `core.go` holds the immutable `*RoutingState` behind an RWMutex;
  `swap.go` has the hot-switch rebuilds. **This is the concurrency heart — see below.**
- `repo` / `store` — SQLite access + embedded migrations.
- `api` — chi REST surface (loopback, also reused behind the web panel's cookie auth).
- `web` — LAN panel (cookie session) + serves the public `/subscription`.
- `crypto`, `auth` (deny-list / rate-limit), `registry` (live connections),
  `webshare` (API client), `sync` (Webshare resync), `latency`, `ws` (SSE/WS hub).
- `ui/PIO` — SwiftUI app (spawns/talks to the daemon over loopback).
- `extension/` — Chrome MV3 proxy-switcher that consumes a `?type=http` subscription.

### Routing is RCU/COW — this is the core invariant

`routing.Core` holds one `*RoutingState` pointer (users, upstreams, display-name
index, universal password). Readers call `Snapshot()` (RLock, copy pointer, RUnlock)
then operate lock-free on the immutable struct. Writers build a **completely new**
`RoutingState` off-lock, then `Swap()` it in under a microsecond-held write lock.
Never mutate a field reachable from a snapshot — allocate a new state instead.

**Lock order (declared in `routing/routing.go`, asserted under `-tags=lockaudit`):**
`mappingChangeMu` > `routingMu` > `sql.Tx`. The advisory failure-counter lock is a
leaf (never held while taking another). If you take a SQLite tx, do **not** then grab
`routingMu`; if you hold `routingMu`, do **not** open a tx. The `internal/lockaudit`
package is a no-op in production builds and a panic-on-violation harness under the
build tag — run `go test -tags=lockaudit ./...` after touching locking code.

**Hot-switch teardown** (`swap.go`): every `(user→upstream)` mapping carries a
`CancelGroup`. Remapping/editing/deleting tears down in-flight connections to the old
target within ~1 TCP RTT by: persist to SQLite first (durability fence) → build + swap
new state → cancel the OLD group OUTSIDE all locks. `Bridge` reacts to cancellation by
setting a past deadline on both sockets to unblock the copy goroutines. The registry
side-effect (`CloseByUserUpstream`) is injected as a callback so `routing` doesn't
import `registry`. Three rebuild entry points: `SwapUserMapping` (one user),
`RebuildAfterSync` (rotate CG only on new brokenness), `RebuildForUpstreamChange`
(rotate CG for every user on the edited upstream).

### Two auth paths in `tunnel.Acquire` (precedence matters)

1. **Per-user**: `username` is a `local_users` row and `password` matches that user's
   own password → route to the user's mapped upstream.
2. **Universal**: a daemon-wide universal password matches → treat `username` as an
   upstream **display name** and route by it. Ambiguous display names (shared by 2+
   upstreams) are dropped from the index entirely, never silently routed.

All password compares are constant-time (`crypto/subtle`). The built-in **`default`**
upstream (historically `direct`, renamed in migration 0011) egresses from the daemon's
own host (no upstream hop), is immutable, reachable by name, and offered as a mapping
target in the admin UI (`api.listUpstreams` returns it; it has no owning key so it never
shows under a key's table) — note the wider-egress security caveat in README.

## Conventions & gotchas

- **Module path is `github.com/guofan/pio`.** The project was renamed PIA → PIO; do not
  reintroduce `pia`. **Exception:** the crypto AAD prefix is intentionally frozen as the
  historical `"webshare-proxy/v1/"` (`internal/crypto/aesgcm.go`) so existing ciphertext
  stays decryptable — never "fix" it. Likewise the external Webshare.io vendor name stays
  `webshare`.
- **CGO-free SQLite** (`modernc.org/sqlite`): always build with `CGO_ENABLED=0`. This is
  what lets the Docker image be static and the app binary portable.
- **Migrations**: plain `.sql` under `internal/store/migrations/`, applied in
  lexicographic order, recorded in `schema_migrations`. Add the next-numbered file; never
  edit a shipped one.
- **Secrets at rest**: API keys, upstream passwords, and the universal password are
  AES-256-GCM encrypted with `<data-dir>/master.key` (mode 0600). Local user passwords are
  plaintext by design (the UI reveals them).
- **Integration tests** (`test/integration/`) drive real listeners against
  `test/mockwebshare/`; `main_test.go` runs `goleak` once per package (not per test) —
  goroutine leaks fail the suite, so always thread ctx/cancel through new goroutines.
- The daemon keeps the REST surface up even if the proxy listener fails to bind on boot
  (so the UI can show the error) — proxy bring-up failures are logged, not fatal; web-panel
  bind failures ARE fatal.
