# PIO Proxy Switcher (Chrome extension)

A Manifest V3 Chrome extension that consumes a [PIO](../README.md) subscription
URL, lists the available proxies, and applies a chosen one **browser-wide** —
authenticating the proxy automatically so every tab routes through it.

## Why HTTP proxies

Chrome cannot embed credentials in a proxy configuration, and it cannot
authenticate **SOCKS** proxies at all. So this extension fetches the
subscription with `?type=http` (added to the daemon in
[`internal/api/subscription.go`](../internal/api/subscription.go)) and answers
the proxy's `407` challenge via `chrome.webRequest.onAuthRequired`. PIO's
unified proxy port speaks both HTTP and SOCKS, so `type=http` reaches the exact
same routing set — it only changes the URI scheme.

## How it works

```
popup.js ──(applyProxy)──▶ background.js ──▶ chrome.proxy.settings (fixed_servers)
   ▲                            │            └▶ chrome.action badge (which proxy)
   │                            └──▶ chrome.storage.local { activeProxy }
   │                                          ▲
   └── fetch(<sub>?type=http) ── parse ──▶    │
                                              │
   onAuthRequired (proxy 407) ───────────────┘  supplies username/password
```

- **`lib/parse.js`** — pure, dependency-free parser for subscription lines
  (`<scheme>://<name>:<password>@<host>:<port>#<name>`). Unit-tested.
- **`popup.{html,css,js}`** — save/refresh/remove the single subscription and
  pick a proxy, or **Disable** to go direct. Saving a new URL replaces the
  existing subscription.
- **`background.js`** — sets `chrome.proxy`, supplies proxy credentials on
  `onAuthRequired`, and reflects the active proxy on the toolbar icon (a green
  badge + the proxy name in the tooltip; cleared when disabled). The active
  proxy is persisted so the listener keeps working after the service worker is
  recycled.

### Immediate switching

Every PIO proxy is reached on the **same** unified host:port — only the
display-name credential differs. Chrome caches the proxy credentials it first
authenticated with (keyed by the proxy endpoint) and keeps re-sending them, so
naively re-pointing at the identical proxy leaves open pages on the **old**
upstream until a reload (sometimes longer). On every switch `background.js`
therefore calls `chrome.browsingData.remove` **scoped to the proxy's own
origin**, which clears just that one auth-cache entry — the next request
re-challenges and authenticates as the newly-selected proxy, so the switch is
immediate. The scope means real site cookies are never touched.

## Install (unpacked)

1. Open `chrome://extensions`.
2. Enable **Developer mode** (top-right).
3. Click **Load unpacked** and select this `extension/` directory.
4. Click the extension icon, paste your subscription URL (the one from the web
   panel's "Copy subscription URL" button), and **Save**.
5. Pick a proxy from the list. The pill turns **On** and the toolbar icon shows
   a green badge with the proxy's short name. Use **Disable** to stop using a
   proxy (the badge clears).

> The extension always fetches with `type=http`; you can paste either the
> `socks` or `http` form of the URL — the `type` parameter is normalized
> ([`lib/parse.js`](lib/parse.js) `withType`). After you **edit** the
> extension's files, click **Reload** on `chrome://extensions` — service workers
> keep running the old code until reloaded (see [Troubleshooting](#troubleshooting)).

## Packaging

Unpacked loading (above) is enough for everyday use. To produce a distributable
archive — for the Chrome Web Store or to hand someone a single file — run:

```sh
./scripts/build-extension.sh             # → dist/pio-extension-<version>.zip
./scripts/build-extension.sh /tmp/out    # custom output dir
```

The script zips **only** the runtime files — `manifest.json`, `background.js`,
`popup.{html,css,js}`, `lib/parse.js` — with `manifest.json` at the archive root
(Chrome rejects a nested manifest), leaving out `e2e/`, `test/`, `node_modules/`,
this README, and any local state. The version in the filename is read straight
from `manifest.json`, so bump its `"version"` before cutting a release.

- **Chrome Web Store:** upload the `.zip` in the Developer Dashboard.
- **Self-hosted `.crx`** (generate a signed package + a stable key):

  ```sh
  unzip -o dist/pio-extension-*.zip -d dist/extension
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    --pack-extension=dist/extension \
    --pack-extension-key=dist/extension.pem   # omit on the FIRST run to mint the key
  # → dist/extension.crx  (+ dist/extension.pem the first time)
  ```

  Keep `extension.pem` private — it is the extension's identity; reuse the same
  key for every update or the ID changes. Note that current Chrome only installs
  a `.crx` via enterprise policy; for personal/dev use, **Load unpacked** is the
  path. Both `dist/` outputs are git-ignored.

## Tests

```sh
# Unit tests (pure parser) — no browser needed
node --test extension/test/parse.test.mjs   # or: cd extension && node --test

# End-to-end (loads the unpacked extension in Chromium, drives the popup,
# and asserts chrome.proxy + credentials are applied). Requires Playwright;
# kept out of test/ so `node --test` does not try to launch a browser.
node extension/e2e/e2e.mjs
```

## Troubleshooting

**Enabling a proxy makes every page fail to load.** The extension can only drive
**HTTP** proxies — Chrome cannot authenticate SOCKS — so it always fetches the
subscription as `?type=http` and applies an `http`-scheme proxy. If pages load
only when your pasted URL already contains `type=http` and break for the `socks`
(or no-`type`) form, the copy running in Chrome is **stale**: a SOCKS-scheme
proxy slipped through and Chrome 407s every request. Extension service workers
keep executing old code until reloaded, so:

1. `chrome://extensions` → **Reload** the PIO extension.
2. Reopen the popup, then **remove and re-add** the subscription.
3. Re-select a proxy and retry — a `socks`/no-`type` URL now works too.

The fix is in the running build when [`popup.js`](popup.js)'s `fetchSubscription`
calls `fetch(withType(sub.url, 'http'), …)` — that forces `type=http` regardless
of what you pasted.

## Permissions

| Permission | Why |
| --- | --- |
| `proxy` | Set the browser-wide proxy. |
| `storage` | Persist the subscription + the active proxy. |
| `webRequest`, `webRequestAuthProvider` | Answer the proxy `407` with credentials. |
| `browsingData` | Flush the proxy endpoint's cached credentials on a switch (scoped to the proxy origin) so the new proxy takes effect immediately. |
| `host_permissions: <all_urls>` | Fetch the subscription and authenticate proxy requests on any site. |
