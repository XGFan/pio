// Service worker: owns the browser-wide proxy setting, reflects the active
// proxy on the toolbar icon, and supplies per-proxy credentials for HTTP proxy
// authentication.
//
// Chrome cannot embed credentials in a proxy config, and it cannot authenticate
// SOCKS proxies at all — so the extension consumes PIO's `type=http` list and
// answers the proxy's 407 challenge here via webRequest.onAuthRequired. The
// active proxy (including its username/password) lives in chrome.storage.local
// so this listener still works after the service worker is torn down and
// respawned.

const STORAGE_KEY = 'activeProxy';

// Request IDs for which we have already offered proxy credentials in the
// current auth attempt. PIO's proxy guards the universal password with a
// per-IP deny-list (10 failures / 60s → 5-min ban), so re-supplying rejected
// credentials on every retry would get the client banned. We offer once per
// request, then decline.
const credentialedRequests = new Set();

// badgeAbbrev derives a short, toolbar-sized label from a proxy display name:
// up to three uppercased letters/digits (e.g. "US-A-01" → "USA"). The filter is
// Unicode-aware so non-Latin names ("日本-東京" → "日本東") still abbreviate to
// something distinguishing rather than collapsing to the "ON" fallback, which
// only applies when the name has no letters/digits at all (badge never blank
// while a proxy is active). The full name is always in the tooltip.
function badgeAbbrev(name) {
  const compact = String(name || '').replace(/[^\p{L}\p{N}]/gu, '').toUpperCase();
  return compact.slice(0, 3) || 'ON';
}

// reflectState paints the toolbar icon to match the active proxy: a green
// badge + abbreviated name + full-name tooltip when proxied, cleared badge +
// "Disabled" tooltip when direct. This is the at-a-glance "am I proxied, and
// through which proxy?" indicator (the popup shows the same state in detail).
async function reflectState(proxy) {
  if (proxy && proxy.host) {
    await chrome.action.setBadgeBackgroundColor({ color: '#22c55e' });
    await chrome.action.setBadgeText({ text: badgeAbbrev(proxy.name) });
    await chrome.action.setTitle({ title: `PIO — ${proxy.name}` });
  } else {
    await chrome.action.setBadgeText({ text: '' });
    await chrome.action.setTitle({ title: 'PIO — Disabled (direct connection)' });
  }
}

// fixedServersValue builds the chrome.proxy setting that points the whole
// browser at one proxy, keeping loopback direct so the popup and local tooling
// are never trapped behind a dead proxy.
function fixedServersValue(proxy) {
  return {
    mode: 'fixed_servers',
    rules: {
      singleProxy: {
        scheme: proxy.scheme,
        host: proxy.host,
        port: proxy.port,
      },
      bypassList: ['localhost', '127.0.0.1', '[::1]', '<local>'],
    },
  };
}

// proxyOrigin is the origin Chrome keys its proxy auth-cache entry under: the
// proxy endpoint's scheme://host:port. PIO's type=http subscription always
// yields http-scheme proxies; https is handled too for completeness.
function proxyOrigin(proxy) {
  const scheme = proxy.scheme === 'https' ? 'https' : 'http';
  return `${scheme}://${proxy.host}:${proxy.port}`;
}

// applyProxy persists the selected proxy, points the whole browser at it, and
// flushes Chrome's cached proxy credentials so the switch takes effect at once.
//
// Every PIO proxy is reached on the SAME unified host:port — they differ only
// by the display-name we answer onAuthRequired with. Chrome caches the proxy
// credentials it first authenticated with (keyed by the proxy endpoint) and
// re-sends them on every later request — even over fresh connections — so a
// plain re-set of an identical singleProxy keeps routing through the OLD
// upstream until the auth cache is invalidated (the "switch only works after a
// reload, sometimes not even then" bug). browsingData.remove scoped to the
// proxy's own origin clears exactly that auth-cache entry (the proxy endpoint
// serves no site cookies, so nothing the user cares about is touched); the next
// request then re-challenges and re-auths as the newly-selected proxy. Storage
// is written first so onAuthRequired already has the new credentials.
async function applyProxy(proxy) {
  await chrome.storage.local.set({ [STORAGE_KEY]: proxy });
  await chrome.proxy.settings.set({ scope: 'regular', value: fixedServersValue(proxy) });
  // Paint the toolbar before the (best-effort) flush so the icon always tracks
  // the selected proxy even if the flush below fails.
  await reflectState(proxy);
  // Flush this endpoint's cached proxy credentials so the switch is immediate.
  // http(s) only — SOCKS has no HTTP auth cache, and the subscription is always
  // fetched as type=http so this is the live path. The clear relies on an
  // undocumented coupling (origin-scoped cookie removal also drops the proxy
  // auth-cache entry); e2e/e2e.mjs is the canary if a future Chrome decouples
  // them. A failure here degrades to "switch applies on next reload", not a
  // broken switch, so we log and carry on.
  if (proxy.scheme === 'http' || proxy.scheme === 'https') {
    try {
      await chrome.browsingData.remove({ origins: [proxyOrigin(proxy)] }, { cookies: true });
    } catch (e) {
      console.warn('[PIO] proxy auth-cache flush failed; switch may need a reload:', e);
    }
  }
}

// clearProxy returns the browser to a direct (no-proxy) connection.
async function clearProxy() {
  await chrome.storage.local.set({ [STORAGE_KEY]: null });
  await chrome.proxy.settings.set({ scope: 'regular', value: { mode: 'direct' } });
  await reflectState(null);
}

// syncBadgeFromStorage repaints the icon from persisted state. The service
// worker is recycled aggressively, and badge/title are reset on respawn, so we
// re-derive them whenever the worker boots.
async function syncBadgeFromStorage() {
  const data = await chrome.storage.local.get(STORAGE_KEY);
  await reflectState(data[STORAGE_KEY] || null);
}
syncBadgeFromStorage();
chrome.runtime.onStartup.addListener(syncBadgeFromStorage);
chrome.runtime.onInstalled.addListener(syncBadgeFromStorage);

// onAuthRequired fires for every 407/401 challenge. We answer ONLY proxy
// challenges (details.isProxy) and only with the active proxy's credentials;
// site logins are left for the browser/user to handle. If the proxy rejects
// our credentials it re-fires for the same requestId — we then decline rather
// than re-sending the same bad credentials (see credentialedRequests above).
chrome.webRequest.onAuthRequired.addListener(
  (details, callback) => {
    if (!details.isProxy) {
      callback({});
      return;
    }
    if (credentialedRequests.has(details.requestId)) {
      credentialedRequests.delete(details.requestId);
      callback({ cancel: true });
      return;
    }
    chrome.storage.local.get(STORAGE_KEY).then((data) => {
      const active = data[STORAGE_KEY];
      if (active && active.username) {
        credentialedRequests.add(details.requestId);
        callback({
          authCredentials: {
            username: active.username,
            password: active.password || '',
          },
        });
      } else {
        callback({});
      }
    });
  },
  { urls: ['<all_urls>'] },
  ['asyncBlocking'],
);

// Forget request IDs once they finish so credentialedRequests stays bounded.
const forgetRequest = (details) => credentialedRequests.delete(details.requestId);
chrome.webRequest.onCompleted.addListener(forgetRequest, { urls: ['<all_urls>'] });
chrome.webRequest.onErrorOccurred.addListener(forgetRequest, { urls: ['<all_urls>'] });

// Surface proxy-level failures (unreachable host, refused auth) to the console
// for debugging; the popup separately reflects the configured state.
chrome.proxy.onProxyError.addListener((details) => {
  console.warn('[PIO] proxy error:', details.error, details.details);
});

// Popup → worker command channel. Returning true keeps the message port open
// for the async reply.
chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  (async () => {
    try {
      if (msg && msg.type === 'applyProxy') {
        await applyProxy(msg.proxy);
        sendResponse({ ok: true });
      } else if (msg && msg.type === 'setDirect') {
        await clearProxy();
        sendResponse({ ok: true });
      } else {
        sendResponse({ ok: false, error: 'unknown message' });
      }
    } catch (e) {
      sendResponse({ ok: false, error: String(e && e.message ? e.message : e) });
    }
  })();
  return true;
});
