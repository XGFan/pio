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
  filterRow: document.getElementById('filter-row'),
  filter: document.getElementById('filter'),
  subs: document.getElementById('subs'),
  subTpl: document.getElementById('sub-template'),
  proxyTpl: document.getElementById('proxy-template'),
};

let filterText = '';

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

async function goDirect() {
  const resp = await chrome.runtime.sendMessage({ type: 'setDirect' });
  if (!resp || !resp.ok) {
    showAddError(`Could not go direct: ${resp ? resp.error : 'no response'}`);
    return;
  }
  await render();
}

function matchesFilter(proxy) {
  if (!filterText) return true;
  return proxy.name.toLowerCase().includes(filterText.toLowerCase());
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

function renderSubscription(sub, activeProxy) {
  const node = els.subTpl.content.firstElementChild.cloneNode(true);
  node.querySelector('[data-host]').textContent = hostLabel(sub.url);

  const visible = sub.proxies.filter(matchesFilter);
  node.querySelector('[data-count]').textContent =
    sub.proxies.length === visible.length
      ? `${sub.proxies.length} proxies`
      : `${visible.length}/${sub.proxies.length} proxies`;

  const errEl = node.querySelector('[data-error]');
  if (sub.error) {
    errEl.textContent = sub.error;
    errEl.hidden = false;
  }

  node.querySelector('[data-act="refresh"]').addEventListener('click', refreshSubscription);
  node.querySelector('[data-act="remove"]').addEventListener('click', removeSubscription);

  const list = node.querySelector('[data-list]');
  for (const proxy of visible) {
    list.appendChild(renderProxy(proxy, activeProxy));
  }
  return node;
}

async function render() {
  const { subscription, activeProxy } = await getState();

  // Active bar + status pill.
  if (activeProxy) {
    els.activeName.textContent = activeProxy.name;
    els.statusPill.textContent = 'Proxied';
    els.statusPill.className = 'pill pill-active';
  } else {
    els.activeName.textContent = 'No proxy (direct)';
    els.statusPill.textContent = 'Direct';
    els.statusPill.className = 'pill pill-direct';
  }

  els.filterRow.hidden = !(subscription && subscription.proxies.length > 0);

  els.subs.replaceChildren();
  if (!subscription) {
    const empty = document.createElement('div');
    empty.className = 'empty';
    empty.textContent = 'Add a PIA subscription URL to get started.';
    els.subs.appendChild(empty);
    return;
  }
  els.subs.appendChild(renderSubscription(subscription, activeProxy));
}

els.addForm.addEventListener('submit', (e) => {
  e.preventDefault();
  addSubscription(els.subUrl.value.trim());
});
els.btnDirect.addEventListener('click', goDirect);
els.filter.addEventListener('input', () => {
  filterText = els.filter.value.trim();
  render();
});

render();
