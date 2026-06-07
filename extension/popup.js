// Popup controller: manage subscriptions, render the proxy list, and tell the
// service worker which proxy to apply. All durable state lives in
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
  const { subscriptions = [], activeProxy = null } =
    await chrome.storage.local.get(['subscriptions', 'activeProxy']);
  return { subscriptions, activeProxy };
}

async function setSubscriptions(subscriptions) {
  await chrome.storage.local.set({ subscriptions });
}

function showAddError(msg) {
  els.addError.textContent = msg;
  els.addError.hidden = !msg;
}

// hostLabel renders a short, human-friendly label for a subscription source.
function hostLabel(url) {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

// fetchSubscription pulls the HTTP-proxy list for one subscription and stores
// the parsed proxies (or an error message) back into the subscription record.
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

async function addSubscription(rawUrl) {
  showAddError('');
  let normalized;
  try {
    normalized = new URL(rawUrl).toString();
  } catch {
    showAddError('Enter a valid subscription URL.');
    return;
  }
  const { subscriptions } = await getState();
  if (subscriptions.some((s) => s.url === normalized)) {
    showAddError('That subscription is already added.');
    return;
  }
  const sub = {
    id: crypto.randomUUID(),
    url: normalized,
    proxies: [],
    error: '',
  };
  await fetchSubscription(sub);
  subscriptions.push(sub);
  await setSubscriptions(subscriptions);
  els.subUrl.value = '';
  await render();
}

async function refreshSubscription(id) {
  const { subscriptions } = await getState();
  const sub = subscriptions.find((s) => s.id === id);
  if (!sub) return;
  await fetchSubscription(sub);
  await setSubscriptions(subscriptions);
  await render();
}

async function removeSubscription(id) {
  const { subscriptions } = await getState();
  await setSubscriptions(subscriptions.filter((s) => s.id !== id));
  await render();
}

async function selectProxy(subId, proxyId) {
  const { subscriptions } = await getState();
  const sub = subscriptions.find((s) => s.id === subId);
  const proxy = sub && sub.proxies.find((p) => p.id === proxyId);
  if (!proxy) return;
  // The worker writes activeProxy (with subId) in a single set(); the popup
  // just re-renders from storage afterward.
  const resp = await chrome.runtime.sendMessage({ type: 'applyProxy', proxy, subId });
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
  const q = filterText.toLowerCase();
  return (
    proxy.name.toLowerCase().includes(q) ||
    proxy.host.toLowerCase().includes(q) ||
    String(proxy.port).includes(q)
  );
}

function renderProxy(sub, proxy, activeProxy) {
  const node = els.proxyTpl.content.firstElementChild.cloneNode(true);
  const isActive =
    activeProxy && activeProxy.subId === sub.id && activeProxy.id === proxy.id;
  if (isActive) node.classList.add('selected');
  node.querySelector('[data-name]').textContent = proxy.name;
  node.querySelector('[data-endpoint]').textContent =
    `${proxy.rawScheme} · ${proxy.host}:${proxy.port}`;
  node
    .querySelector('.proxy-btn')
    .addEventListener('click', () => selectProxy(sub.id, proxy.id));
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

  node
    .querySelector('[data-act="refresh"]')
    .addEventListener('click', () => refreshSubscription(sub.id));
  node
    .querySelector('[data-act="remove"]')
    .addEventListener('click', () => removeSubscription(sub.id));

  const list = node.querySelector('[data-list]');
  for (const proxy of visible) {
    list.appendChild(renderProxy(sub, proxy, activeProxy));
  }
  return node;
}

async function render() {
  const { subscriptions, activeProxy } = await getState();

  // Active bar + status pill.
  if (activeProxy) {
    els.activeName.textContent = `${activeProxy.name} · ${activeProxy.host}:${activeProxy.port}`;
    els.statusPill.textContent = 'Proxied';
    els.statusPill.className = 'pill pill-active';
  } else {
    els.activeName.textContent = 'No proxy (direct)';
    els.statusPill.textContent = 'Direct';
    els.statusPill.className = 'pill pill-direct';
  }

  const totalProxies = subscriptions.reduce((n, s) => n + s.proxies.length, 0);
  els.filterRow.hidden = totalProxies === 0;

  els.subs.replaceChildren();
  if (subscriptions.length === 0) {
    const empty = document.createElement('div');
    empty.className = 'empty';
    empty.textContent = 'Add a PIA subscription URL to get started.';
    els.subs.appendChild(empty);
    return;
  }
  for (const sub of subscriptions) {
    els.subs.appendChild(renderSubscription(sub, activeProxy));
  }
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
