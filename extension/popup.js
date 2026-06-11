// Popup controller: manage the single subscription, render the proxy list, and
// tell the service worker which proxy to apply. All durable state lives in
// chrome.storage.local; the popup re-renders from storage after every action so
// the worker and popup never disagree.

import { parseSubscription, withType } from './lib/parse.js';

const els = {
  statusPill: document.getElementById('status-pill'),
  activeName: document.getElementById('active-name'),
  btnDirect: document.getElementById('btn-direct'),
  addForm: document.getElementById('add-form'),
  subUrl: document.getElementById('sub-url'),
  addError: document.getElementById('add-error'),
  subs: document.getElementById('subs'),
  proxyTpl: document.getElementById('proxy-template'),
};

async function getState() {
  const { subscription = null, activeProxy = null } =
    await chrome.storage.local.get(['subscription', 'activeProxy']);
  return { subscription, activeProxy };
}

async function setSubscription(subscription) {
  await chrome.storage.local.set({ subscription });
}

function showAddError(msg) {
  els.addError.textContent = msg;
  els.addError.hidden = !msg;
}

// hostLabel renders a short, human-friendly label for the subscription source.
function hostLabel(url) {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

// fetchSubscription pulls the HTTP-proxy list and stores the parsed proxies (or
// an error message) back into the subscription record.
async function fetchSubscription(sub) {
  try {
    const res = await fetch(withType(sub.url, 'http'), { cache: 'no-store' });
    if (!res.ok) {
      sub.error =
        res.status === 401
          ? 'Auth failed — check the password in the URL.'
          : res.status === 404
            ? 'Not found — subscription disabled or no universal password.'
            : `Fetch failed (HTTP ${res.status}).`;
      sub.proxies = [];
      return;
    }
    const text = await res.text();
    sub.proxies = parseSubscription(text);
    sub.error = sub.proxies.length === 0 ? 'No proxies in this subscription.' : '';
  } catch (e) {
    sub.error = `Fetch error: ${e && e.message ? e.message : e}`;
    sub.proxies = sub.proxies || [];
  }
}

// addSubscription sets (or replaces) the single subscription. Only one is
// supported, so a new URL overwrites whatever was there before.
async function addSubscription(rawUrl) {
  showAddError('');
  let normalized;
  try {
    normalized = new URL(rawUrl).toString();
  } catch {
    showAddError('Enter a valid subscription URL.');
    return;
  }
  const sub = { url: normalized, proxies: [], error: '' };
  await fetchSubscription(sub);
  await setSubscription(sub);
  els.subUrl.value = '';
  await render();
}

async function refreshSubscription() {
  const { subscription } = await getState();
  if (!subscription) return;
  await fetchSubscription(subscription);
  await setSubscription(subscription);
  await render();
}

async function removeSubscription() {
  await setSubscription(null);
  await render();
}

async function selectProxy(proxyId) {
  const { subscription } = await getState();
  const proxy = subscription && subscription.proxies.find((p) => p.id === proxyId);
  if (!proxy) return;
  // The worker writes activeProxy in a single set(); the popup just re-renders
  // from storage afterward.
  const resp = await chrome.runtime.sendMessage({ type: 'applyProxy', proxy });
  if (!resp || !resp.ok) {
    showAddError(`Could not apply proxy: ${resp ? resp.error : 'no response'}`);
    return;
  }
  await render();
}

// disableProxy turns the proxy off (direct connection). This is the "Disable"
// control; the worker resets chrome.proxy to direct and clears the badge.
async function disableProxy() {
  const resp = await chrome.runtime.sendMessage({ type: 'setDirect' });
  if (!resp || !resp.ok) {
    showAddError(`Could not disable proxy: ${resp ? resp.error : 'no response'}`);
    return;
  }
  await render();
}

function renderProxy(proxy, activeProxy) {
  const node = els.proxyTpl.content.firstElementChild.cloneNode(true);
  if (activeProxy && activeProxy.id === proxy.id) node.classList.add('selected');
  node.querySelector('[data-name]').textContent = proxy.name;
  node
    .querySelector('.proxy-btn')
    .addEventListener('click', () => selectProxy(proxy.id));
  return node;
}

// renderSubscription builds the single subscription block: a source row (host,
// count, refresh, remove) followed by the proxy list. With only one
// subscription there is no outer list to nest in — this renders straight into
// the main area.
function renderSubscription(sub, activeProxy) {
  const node = document.createElement('div');
  node.className = 'sub';

  const head = document.createElement('div');
  head.className = 'sub-head';
  head.innerHTML = `
    <div class="sub-meta">
      <span class="sub-host" data-host></span>
      <span class="sub-count" data-count></span>
    </div>
    <div class="sub-actions">
      <button class="btn btn-icon" data-act="refresh" title="Refresh">↻</button>
      <button class="btn btn-icon" data-act="remove" title="Remove">✕</button>
    </div>`;
  head.querySelector('[data-host]').textContent = hostLabel(sub.url);
  head.querySelector('[data-count]').textContent =
    `${sub.proxies.length} ${sub.proxies.length === 1 ? 'proxy' : 'proxies'}`;
  head.querySelector('[data-act="refresh"]').addEventListener('click', refreshSubscription);
  head.querySelector('[data-act="remove"]').addEventListener('click', removeSubscription);
  node.appendChild(head);

  if (sub.error) {
    const errEl = document.createElement('p');
    errEl.className = 'sub-error error';
    errEl.textContent = sub.error;
    node.appendChild(errEl);
  }

  const list = document.createElement('ul');
  list.className = 'proxy-list';
  for (const proxy of sub.proxies) {
    list.appendChild(renderProxy(proxy, activeProxy));
  }
  node.appendChild(list);
  return node;
}

async function render() {
  const { subscription, activeProxy } = await getState();

  // Active bar + status pill. The Disable button only shows when a proxy is on.
  if (activeProxy) {
    els.activeName.textContent = activeProxy.name;
    els.statusPill.textContent = 'On';
    els.statusPill.className = 'pill pill-active';
    els.btnDirect.hidden = false;
  } else {
    els.activeName.textContent = 'Disabled';
    els.statusPill.textContent = 'Off';
    els.statusPill.className = 'pill pill-direct';
    els.btnDirect.hidden = true;
  }

  els.subs.replaceChildren();
  if (!subscription) {
    const empty = document.createElement('div');
    empty.className = 'empty';
    empty.textContent = 'Add a PIO subscription URL above to get started.';
    els.subs.appendChild(empty);
    return;
  }
  els.subs.appendChild(renderSubscription(subscription, activeProxy));
}

els.addForm.addEventListener('submit', (e) => {
  e.preventDefault();
  addSubscription(els.subUrl.value.trim());
});
els.btnDirect.addEventListener('click', disableProxy);

render();
