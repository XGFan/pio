// End-to-end test for the PIO Proxy Switcher extension.
//
// It proves the full chain, not just the UI:
//   1. A local server serves a PIO-style subscription (and asserts the
//      extension requested it with ?type=http).
//   2. A local forward proxy REQUIRES Basic auth (407 without it).
//   3. The extension is loaded unpacked in Chromium; the popup adds the
//      subscription, lists the proxy, and the user selects it.
//   4. chrome.proxy.settings is verified to be fixed_servers → our proxy.
//   5. A page navigated through the proxy only succeeds because
//      onAuthRequired supplied the right credentials (the proxy 407s
//      otherwise) — so a sentinel body proves the whole auth path works.
//   6. "Go direct" resets the browser to a direct connection.
//
// Run: node extension/test/e2e.mjs   (Playwright must be installed)

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

const USER = 'US-A-01';
const PASS = 'p@ss:w0rd'; // includes : and @ to exercise percent-decoding
const SENTINEL = 'OK-THROUGH-PROXY';

function listen(server) {
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server.address().port)));
}

function assert(cond, msg) {
  if (!cond) throw new Error(`ASSERT FAILED: ${msg}`);
  console.log(`  ✓ ${msg}`);
}

async function main() {
  let sawTypeHttp = false;

  // (1) Subscription server — emits one http-proxy line pointing at our proxy.
  // Returns a different proxy name when the URL carries "v2", so the test can
  // prove a second subscription URL REPLACES the first (single-subscription).
  const subServer = http.createServer((req, res) => {
    if (req.url.includes('type=http')) sawTypeHttp = true;
    const name = req.url.includes('v2') ? 'US-B-02' : USER;
    const line = `http://${encodeURIComponent(name)}:${encodeURIComponent(PASS)}@127.0.0.1:${proxyPort}#${name}`;
    res.writeHead(200, { 'Content-Type': 'text/plain' });
    res.end(line + '\n');
  });

  // (2) Authenticating forward proxy.
  const expectedAuth = 'Basic ' + Buffer.from(`${USER}:${PASS}`).toString('base64');
  const proxyServer = http.createServer((req, res) => {
    if (req.headers['proxy-authorization'] !== expectedAuth) {
      res.writeHead(407, {
        'Proxy-Authenticate': 'Basic realm="pio"',
        'Content-Type': 'text/plain',
        Connection: 'close',
      });
      res.end('proxy auth required');
      return;
    }
    res.writeHead(200, { 'Content-Type': 'text/html', Connection: 'close' });
    res.end(`<html><body>${SENTINEL} ${req.url}</body></html>`);
  });

  const subPort = await listen(subServer);
  const proxyPort = await listen(proxyServer);

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

  try {
    let [sw] = context.serviceWorkers();
    if (!sw) sw = await context.waitForEvent('serviceworker', { timeout: 15000 });
    const extId = new URL(sw.url()).host;
    console.log(`Extension id: ${extId}`);

    const popup = await context.newPage();
    await popup.goto(`chrome-extension://${extId}/popup.html`);

    // (3) Add subscription + select the proxy.
    await popup.fill('#sub-url', `http://127.0.0.1:${subPort}/subscription?password=secret`);
    await popup.click('#add-form button[type="submit"]');
    await popup.waitForSelector('.proxy', { timeout: 10000 });

    const proxyName = await popup.textContent('.proxy-name');
    assert(proxyName.trim() === USER, `proxy "${USER}" listed in popup`);
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

    await popup.click('.proxy-btn');
    await popup.waitForSelector('.proxy.selected', { timeout: 5000 });
    assert(true, 'proxy row marked selected after click');

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
    assert(stored.activeProxy?.username === USER, 'active username persisted');
    assert(stored.activeProxy?.password === PASS, 'active password persisted (decoded)');

    // Active bar shows the name only — no host:port.
    const activeText = (await popup.textContent('#active-name')).trim();
    assert(activeText === USER, 'active bar shows name only (no host:port)');

    // (5) Real traffic through the proxy succeeds ONLY via onAuthRequired creds.
    const target = await context.newPage();
    await target.goto('http://pio-e2e.test/hello', { timeout: 15000 });
    const body = await target.content();
    assert(body.includes(SENTINEL), 'page loaded through authenticated proxy (onAuthRequired worked)');
    assert(body.includes('/hello'), 'proxy received the real request path');
    await target.close();

    // (6) Go direct.
    await popup.bringToFront();
    await popup.click('#btn-direct');
    await popup.waitForSelector('.pill-direct', { timeout: 5000 });
    const after = await popup.evaluate(
      () => new Promise((r) => chrome.proxy.settings.get({}, r)),
    );
    assert(after.value?.mode === 'direct', 'browser returned to direct after Go direct');

    // (7) A second subscription URL REPLACES the first (single subscription).
    await popup.fill('#sub-url', `http://127.0.0.1:${subPort}/subscription?password=secret&v2=1`);
    await popup.click('#add-form button[type="submit"]');
    await popup.waitForFunction(
      () => {
        const names = [...document.querySelectorAll('.proxy-name')].map((e) => e.textContent.trim());
        return names.length === 1 && names[0] === 'US-B-02';
      },
      { timeout: 10000 },
    );
    const subBlocks = await popup.$$eval('.sub', (els) => els.length);
    assert(subBlocks === 1, 'only one subscription block after adding a second URL');
    const allNames = await popup.$$eval('.proxy-name', (els) => els.map((e) => e.textContent.trim()));
    assert(
      allNames.length === 1 && allNames[0] === 'US-B-02' && !allNames.includes(USER),
      'second subscription replaced the first (old proxy gone)',
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
