// Service worker: owns the browser-wide proxy setting and supplies per-proxy
// credentials for HTTP proxy authentication.
//
// Chrome cannot embed credentials in a proxy config, and it cannot authenticate
// SOCKS proxies at all — so the extension consumes PIA's `type=http` list and
// answers the proxy's 407 challenge here via webRequest.onAuthRequired. The
// active proxy (including its username/password) lives in chrome.storage.local
// so this listener still works after the service worker is torn down and
// respawned.

const STORAGE_KEY = 'activeProxy';

// Request IDs for which we have already offered proxy credentials in the
// current auth attempt. PIA's proxy guards the universal password with a
// per-IP deny-list (10 failures / 60s → 5-min ban), so re-supplying rejected
// credentials on every retry would get the client banned. We offer once per
// request, then decline.
const credentialedRequests = new Set();

// applyProxy persists the selected proxy and points the whole browser at it.
// subId records which subscription it came from so the popup can highlight the
// active row; it is written in the SAME set() as the credentials (single
// writer) to avoid an inconsistent intermediate state.
async function applyProxy(proxy, subId) {
  await chrome.storage.local.set({ [STORAGE_KEY]: { ...proxy, subId } });
  await chrome.proxy.settings.set({
    scope: 'regular',
    value: {
      mode: 'fixed_servers',
      rules: {
        singleProxy: {
          scheme: proxy.scheme,
          host: proxy.host,
          port: proxy.port,
        },
        // Keep loopback direct so the extension popup and local tooling are
        // never trapped behind a dead proxy.
        bypassList: ['localhost', '127.0.0.1', '[::1]', '<local>'],
      },
    },
  });
}

// clearProxy returns the browser to a direct (no-proxy) connection.
async function clearProxy() {
  await chrome.storage.local.set({ [STORAGE_KEY]: null });
  await chrome.proxy.settings.set({
    scope: 'regular',
    value: { mode: 'direct' },
  });
}

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
  console.warn('[PIA] proxy error:', details.error, details.details);
});

// Popup → worker command channel. Returning true keeps the message port open
// for the async reply.
chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  (async () => {
    try {
      if (msg && msg.type === 'applyProxy') {
        await applyProxy(msg.proxy, msg.subId);
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
