# PIA Proxy Switcher (Chrome extension)

A Manifest V3 Chrome extension that consumes a [PIA](../README.md) subscription
URL, lists the available proxies, and applies a chosen one **browser-wide** —
authenticating the proxy automatically so every tab routes through it.

## Why HTTP proxies

Chrome cannot embed credentials in a proxy configuration, and it cannot
authenticate **SOCKS** proxies at all. So this extension fetches the
subscription with `?type=http` (added to the daemon in
[`internal/api/subscription.go`](../internal/api/subscription.go)) and answers
the proxy's `407` challenge via `chrome.webRequest.onAuthRequired`. PIA's
unified proxy port speaks both HTTP and SOCKS, so `type=http` reaches the exact
same routing set — it only changes the URI scheme.

## How it works

```
popup.js ──(applyProxy)──▶ background.js ──▶ chrome.proxy.settings (fixed_servers)
   ▲                            │
   │                            └──▶ chrome.storage.local { activeProxy }
   │                                          ▲
   └── fetch(<sub>?type=http) ── parse ──▶    │
                                              │
   onAuthRequired (proxy 407) ───────────────┘  supplies username/password
```

- **`lib/parse.js`** — pure, dependency-free parser for subscription lines
  (`<scheme>://<name>:<password>@<host>:<port>#<name>`). Unit-tested.
- **`popup.{html,css,js}`** — add/refresh/remove subscriptions, filter and pick
  a proxy, or go direct.
- **`background.js`** — sets `chrome.proxy` and supplies proxy credentials on
  `onAuthRequired`. The active proxy is persisted so the listener keeps working
  after the service worker is recycled.

## Install (unpacked)

1. Open `chrome://extensions`.
2. Enable **Developer mode** (top-right).
3. Click **Load unpacked** and select this `extension/` directory.
4. Click the extension icon, paste your subscription URL (the one from the web
   panel's "Copy subscription URL" button), and **Add**.
5. Pick a proxy from the list. The pill turns **Proxied**. Use **Go direct** to
   stop using a proxy.

> The extension always fetches with `type=http`; you can paste either the
> `socks` or `http` form of the URL — the `type` parameter is normalized.

## Tests

```sh
# Unit tests (pure parser) — no browser needed
node --test extension/test/parse.test.mjs   # or: cd extension && node --test

# End-to-end (loads the unpacked extension in Chromium, drives the popup,
# and asserts chrome.proxy + credentials are applied). Requires Playwright;
# kept out of test/ so `node --test` does not try to launch a browser.
node extension/e2e/e2e.mjs
```

## Permissions

| Permission | Why |
| --- | --- |
| `proxy` | Set the browser-wide proxy. |
| `storage` | Persist subscriptions + the active proxy. |
| `webRequest`, `webRequestAuthProvider` | Answer the proxy `407` with credentials. |
| `host_permissions: <all_urls>` | Fetch the subscription and authenticate proxy requests on any site. |
