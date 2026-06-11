// End-to-end test for the PIO Proxy Switcher extension.
//
// It proves the full chain, not just the UI:
//   1. A local server serves a PIO-style subscription (and asserts the
//      extension requested it with ?type=http).
//   2. A local forward proxy REQUIRES Basic auth (407 without it) and uses
//      keep-alive connections — so it can expose the "switch didn't take
//      effect until reload" bug if the extension fails to drop pooled conns.
//   3. The extension is loaded unpacked in Chromium; the popup adds the
//      subscription, lists the proxies, and the user selects one.
//   4. chrome.proxy.settings is verified to be fixed_servers → our proxy, and
//      the toolbar badge reflects the active proxy (issue 1).
//   5. A page navigated through the proxy only succeeds because
//      onAuthRequired supplied the right credentials (the proxy 407s
//      otherwise) — a sentinel body proves the whole auth path works.
//   6. Switching to a SECOND proxy takes effect immediately on the same
//      (keep-alive) tab without a manual reload — every PIO proxy shares one
//      host:port, so this is the regression test for issue 5.
//   7. "Disable" resets the browser to a direct connection and clears the badge.
//   8. A second subscription URL REPLACES the first (single subscription).
//
// Run: node extension/e2e/e2e.mjs   (Playwright must be installed)

import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { chromium } = require('playwright');

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const EXT_PATH = path.resolve(__dirname, '..');

const PASS = 'p@ss:w0rd'; // includes : and @ to exercise percent-decoding
const PROXY_A = 'US-A-01';
const PROXY_B = 'US-B-02';
const PROXY_C = 'US-C-03'; // only served by the "v2" (replacement) subscription
const KNOWN = new Set([PROXY_A, PROXY_B, PROXY_C]);

function listen(server) {
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server.address().port)));
}

function assert(cond, msg) {
  if (!cond) throw new Error(`ASSERT FAILED: ${msg}`);
  console.log(`  ✓ ${msg}`);
}

async function main() {
  let sawTypeHttp = false;
  let proxyPort;

  const mkLine = (name) =>
    `http://${encodeURIComponent(name)}:${encodeURIComponent(PASS)}@127.0.0.1:${proxyPort}#${name}`;

  // (1) Subscription server. The default list carries TWO proxies on the same
  // host:port (to exercise switching); the "v2" URL returns a single different
  // proxy so we can prove a second URL REPLACES the first.
  const subServer = http.createServer((req, res) => {
    if (req.url.includes('type=http')) sawTypeHttp = true;
    const lines = req.url.includes('v2') ? [mkLine(PROXY_C)] : [mkLine(PROXY_A), mkLine(PROXY_B)];
    res.writeHead(200, { 'Content-Type': 'text/plain' });
    res.end(lines.join('\n') + '\n');
  });

  // (2) Authenticating forward proxy with KEEP-ALIVE connections. It accepts
  // any known display-name + the shared password (mirroring PIO routing by
  // display name) and echoes which user authenticated, so the test can tell
  // WHICH proxy a request actually went through. No `Connection: close`: the
  // socket is reused, which is exactly what makes a naive proxy switch stick to
  // the old upstream until reload.
  const proxyServer = http.createServer((req, res) => {
    const hdr = req.headers['proxy-authorization'] || '';
    const m = /^Basic (.+)$/.exec(hdr);
    const creds = m ? Buffer.from(m[1], 'base64').toString() : '';
    const i = creds.indexOf(':');
    const user = i >= 0 ? creds.slice(0, i) : '';
    const pass = i >= 0 ? creds.slice(i + 1) : '';
    if (!(KNOWN.has(user) && pass === PASS)) {
      res.writeHead(407, { 'Proxy-Authenticate': 'Basic realm="pio"', 'Content-Type': 'text/plain' });
      res.end('proxy auth required');
      return;
    }
    // Set a cookie on the *target site* (not the proxy origin) so the test can
    // prove the scoped auth-cache flush does NOT wipe real site cookies.
    res.writeHead(200, { 'Content-Type': 'text/html', 'Set-Cookie': 'pio_keep=1; Path=/' });
    res.end(`<html><body>THROUGH:${user} ${req.url}</body></html>`);
  });

  const subPort = await listen(subServer);
  proxyPort = await listen(proxyServer);

  const userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pio-ext-e2e-'));
  const context = await chromium.launchPersistentContext(userDataDir, {
    headless: false,
    args: [
      `--disable-extensions-except=${EXT_PATH}`,
      `--load-extension=${EXT_PATH}`,
      '--no-first-run',
      '--no-default-browser-check',
    ],
  });

  let sw;
  try {
    [sw] = context.serviceWorkers();
    if (!sw) sw = await context.waitForEvent('serviceworker', { timeout: 15000 });
    const extId = new URL(sw.url()).host;
    console.log(`Extension id: ${extId}`);

    const popup = await context.newPage();
    await popup.goto(`chrome-extension://${extId}/popup.html`);

    // Extension pages expose chrome.action, so the popup can read the toolbar
    // badge the service worker paints (Worker objects have no waitForFunction).
    const badgeText = () => popup.evaluate(() => chrome.action.getBadgeText({}));
    const waitBadge = (exp) =>
      popup.waitForFunction(async (e) => (await chrome.action.getBadgeText({})) === e, exp);

    // No filter UI any more (issue 2).
    assert((await popup.$('#filter')) === null, 'filter input removed from popup');

    // (3) Add subscription → both proxies listed.
    await popup.fill('#sub-url', `http://127.0.0.1:${subPort}/subscription?password=secret`);
    await popup.click('#add-form button[type="submit"]');
    await popup.waitForFunction(
      () => document.querySelectorAll('.proxy').length === 2,
      { timeout: 10000 },
    );
    const names = await popup.$$eval('.proxy-name', (els) => els.map((e) => e.textContent.trim()));
    assert(names.includes(PROXY_A) && names.includes(PROXY_B), 'both proxies listed in popup');
    assert(sawTypeHttp, 'subscription was fetched with ?type=http');

    // Proxy rows must NOT show the type/scheme or host:port.
    assert((await popup.$('.proxy-endpoint')) === null, 'no proxy-endpoint element rendered');
    const rowText = (await popup.textContent('.proxy')).trim();
    assert(
      !rowText.includes(String(proxyPort)) &&
        !rowText.includes('127.0.0.1') &&
        !/\bhttp\b/i.test(rowText),
      'proxy row hides scheme and host:port',
    );

    // Select proxy A.
    await popup.locator('.proxy', { hasText: PROXY_A }).locator('.proxy-btn').click();
    await popup.locator('.proxy.selected', { hasText: PROXY_A }).waitFor({ timeout: 5000 });
    assert(true, 'proxy A marked selected after click');

    // (4) chrome.proxy.settings reflects the selection.
    const settings = await popup.evaluate(
      () => new Promise((r) => chrome.proxy.settings.get({}, r)),
    );
    const single = settings.value?.rules?.singleProxy;
    assert(settings.value?.mode === 'fixed_servers', 'proxy mode is fixed_servers');
    assert(single?.host === '127.0.0.1' && single?.port === proxyPort,
      `singleProxy points at 127.0.0.1:${proxyPort}`);
    assert(single?.scheme === 'http', 'singleProxy scheme is http');

    const stored = await popup.evaluate(
      () => new Promise((r) => chrome.storage.local.get('activeProxy', r)),
    );
    assert(stored.activeProxy?.username === PROXY_A, 'active username persisted');
    assert(stored.activeProxy?.name === PROXY_A, 'active proxy name (badge/UI source) persisted');
    assert(stored.activeProxy?.password === PASS, 'active password persisted (decoded)');

    // Active bar shows the name only — no host:port — and status pill is On.
    assert((await popup.textContent('#active-name')).trim() === PROXY_A, 'active bar shows name only');
    assert((await popup.$('.pill-active')) !== null, 'status pill shows On (pill-active)');

    // Issue 1: the toolbar badge reflects the active proxy (green abbrev).
    await waitBadge('USA');
    assert((await badgeText()) === 'USA', 'badge shows abbreviation of active proxy A');

    // (5) Real traffic through proxy A succeeds ONLY via onAuthRequired creds.
    const target = await context.newPage();
    await target.goto('http://pio-e2e.test/hello', { timeout: 15000 });
    let body = await target.content();
    assert(body.includes(`THROUGH:${PROXY_A}`), 'page A loaded through authenticated proxy A');
    assert(body.includes('/hello'), 'proxy received the real request path');

    // (6) Issue 5 — switch to proxy B; it must take effect IMMEDIATELY on the
    // same keep-alive tab, with no manual reload of the page that opened the
    // first (proxy-A) connection.
    await popup.bringToFront();
    await popup.locator('.proxy', { hasText: PROXY_B }).locator('.proxy-btn').click();
    await popup.locator('.proxy.selected', { hasText: PROXY_B }).waitFor({ timeout: 5000 });
    await waitBadge('USB');
    assert((await badgeText()) === 'USB', 'badge updates to proxy B after switch');

    await target.goto('http://pio-e2e.test/after-switch', { timeout: 15000 });
    body = await target.content();
    assert(body.includes(`THROUGH:${PROXY_B}`),
      'switch to proxy B took effect immediately (no stale proxy-A connection reused)');

    // The auth-cache flush is scoped to the proxy origin, so the target site's
    // cookie set during the proxy-A load must survive the switch.
    const cookie = await target.evaluate(() => document.cookie);
    assert(cookie.includes('pio_keep=1'), 'real site cookie preserved across the switch');

    await target.close();

    // (7) Disable → direct, badge cleared, button hidden.
    await popup.bringToFront();
    await popup.click('#btn-direct');
    await popup.waitForSelector('.pill-direct', { timeout: 5000 });
    const after = await popup.evaluate(
      () => new Promise((r) => chrome.proxy.settings.get({}, r)),
    );
    assert(after.value?.mode === 'direct', 'browser returned to direct after Disable');
    assert((await popup.textContent('#active-name')).trim() === 'Disabled', 'active bar shows Disabled');
    assert(await popup.$eval('#btn-direct', (b) => b.hidden), 'Disable button hidden when already off');
    await waitBadge('');
    assert((await badgeText()) === '', 'badge cleared when disabled');

    // (8) A second subscription URL REPLACES the first (single subscription).
    await popup.fill('#sub-url', `http://127.0.0.1:${subPort}/subscription?password=secret&v2=1`);
    await popup.click('#add-form button[type="submit"]');
    await popup.waitForFunction(
      () => {
        const ns = [...document.querySelectorAll('.proxy-name')].map((e) => e.textContent.trim());
        return ns.length === 1 && ns[0] === 'US-C-03';
      },
      { timeout: 10000 },
    );
    const subBlocks = await popup.$$eval('.sub', (els) => els.length);
    assert(subBlocks === 1, 'only one subscription block after adding a second URL');
    const allNames = await popup.$$eval('.proxy-name', (els) => els.map((e) => e.textContent.trim()));
    assert(
      allNames.length === 1 && allNames[0] === PROXY_C && !allNames.includes(PROXY_A),
      'second subscription replaced the first (old proxies gone)',
    );

    console.log('\nE2E PASSED ✅');
  } finally {
    await context.close();
    subServer.close();
    proxyServer.close();
    fs.rmSync(userDataDir, { recursive: true, force: true });
  }
}

main().catch((err) => {
  console.error('\nE2E FAILED ❌');
  console.error(err);
  process.exit(1);
});
