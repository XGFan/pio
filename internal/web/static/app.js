'use strict';

// --- State ---

const TAB_STORAGE_KEY = 'webshare.activeTab';

const state = {
  tab: (() => {
    try {
      const saved = localStorage.getItem(TAB_STORAGE_KEY);
      return saved === 'users' || saved === 'sources' ? saved : 'sources';
    } catch (_) { return 'sources'; }
  })(),
  keys: [],
  upstreams: [],
  manualProxies: [],
  users: [],
  settings: null,
  proxy: { running: false, proxy_addr: '' },
  revealedPasswords: {}, // username -> {plaintext, timerId}
  listenerError: '',
};

// --- API helpers ---

async function api(method, path, body) {
  const opts = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(path, opts);
  if (r.status === 401) {
    location.href = '/login';
    throw new Error('unauthorized');
  }
  const text = await r.text();
  let data;
  try { data = text ? JSON.parse(text) : null; } catch (_) { data = text; }
  if (!r.ok) {
    const err = new Error((data && data.error) || ('HTTP ' + r.status));
    err.status = r.status;
    err.data = data;
    throw err;
  }
  return data;
}

const apiGET = (p) => api('GET', p);
const apiPOST = (p, b) => api('POST', p, b ?? {});
const apiPATCH = (p, b) => api('PATCH', p, b);
const apiPUT = (p, b) => api('PUT', p, b);
const apiDELETE = (p) => api('DELETE', p);

// --- Refresh ---

async function refreshAll() {
  try {
    const [keys, upstreams, manualProxies, users, settings, proxy] = await Promise.all([
      apiGET('/api/v1/keys').catch(() => []),
      apiGET('/api/v1/upstreams').catch(() => []),
      apiGET('/api/v1/manual-proxies').catch(() => []),
      apiGET('/api/v1/users').catch(() => []),
      apiGET('/api/v1/settings').catch(() => null),
      apiGET('/api/v1/proxy/status').catch(() => ({ running: false })),
    ]);
    state.keys = keys || [];
    state.upstreams = upstreams || [];
    state.manualProxies = manualProxies || [];
    state.users = users || [];
    if (settings) state.settings = settings;
    state.proxy = proxy || { running: false };
    render();
  } catch (e) {
    // If the very first call returned 401 we already redirected.
    console.error('refresh failed', e);
  }
}

// --- Render ---

const $app = document.getElementById('app');

function render() {
  // Update tab active state
  document.querySelectorAll('#tabs .tab').forEach((b) => {
    b.classList.toggle('active', b.dataset.tab === state.tab);
  });
  if (state.tab === 'sources') renderSources();
  else renderUsers();
}

function renderSources() {
  $app.innerHTML = '';
  const cfgRow = el('div', { class: 'config-row' }, systemSection(), subscriptionSection());
  $app.appendChild(cfgRow);
  $app.appendChild(webshareSection());
  $app.appendChild(manualProxiesSection());
}

function manualProxiesSection() {
  return el('section', {},
    el('h2', {},
      'Manual Proxies',
      el('span', { style: 'flex:1' }),
      el('button', { class: 'icon', title: 'Add manual proxy', onclick: () => openManualProxyModal() }, '+'),
    ),
    state.manualProxies.length === 0
      ? el('div', { class: 'card empty' }, 'No manual proxies yet. Click + to add one.')
      : el('div', { class: 'card', style: 'padding:0' }, renderManualProxiesTable()),
  );
}

function renderManualProxiesTable() {
  const tbody = el('tbody', {}, ...state.manualProxies.map((p) =>
    el('tr', {},
      el('td', {}, p.manual_name || p.display_name),
      el('td', { class: 'mono' }, `${p.host}:${p.port}`),
      el('td', {}, el('span', { class: 'proto-pill' }, (p.protocol || '').toUpperCase())),
      el('td', {}, p.username || el('span', { class: 'muted' }, '—')),
      el('td', {}, latencyCell(p.last_latency_ms)),
      el('td', { class: 'actions' },
        el('div', { class: 'action-group' },
          el('button', { class: 'icon', title: 'Edit', onclick: () => openManualProxyModal(p) }, '✎'),
          el('button', { class: 'icon danger-icon', title: 'Delete', onclick: () => deleteManualProxy(p) }, '✕'),
        ),
      ),
    ),
  ));
  return el('table', { class: 'user-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Name'),
      el('th', {}, 'Host:Port'),
      el('th', {}, 'Protocol'),
      el('th', {}, 'Username'),
      el('th', {}, 'Latency'),
      el('th', { class: 'actions' }, 'Actions'),
    )),
    tbody,
  );
}

async function deleteManualProxy(p) {
  const name = p.manual_name || p.display_name;
  if (!confirm(`Delete manual proxy "${name}"?`)) return;
  try {
    await apiDELETE(`/api/v1/manual-proxies/${encodeURIComponent(p.id)}`);
  } catch (e) {
    if (e.status === 409 && e.data && e.data.referencing_users) {
      const lines = e.data.referencing_users.map((r) => `• ${r.username}`).join('\n');
      alert(`"${name}" is in use by:\n${lines}\nRemap or delete those users first.`);
    } else {
      alert('Delete failed: ' + e.message);
    }
  }
  await refreshAll();
}

// openManualProxyModal handles both add (existing=undefined) and edit modes.
function openManualProxyModal(existing) {
  const root = document.getElementById('modal-root');
  root.innerHTML = '';
  const isEdit = !!existing;

  const nameInput = inputEl({ autofocus: '', value: isEdit ? (existing.manual_name || existing.display_name) : '' });
  const hostInput = inputEl({ value: isEdit ? existing.host : '' });
  const portInput = inputEl({ type: 'number', value: isEdit ? existing.port : '8080', style: 'width:90px' });
  const protocolSelect = selectEl({}, [
    { value: 'http', label: 'HTTP' },
    { value: 'https', label: 'HTTPS' },
    { value: 'socks5', label: 'SOCKS5' },
  ], isEdit ? existing.protocol : 'http');
  const usernameInput = inputEl({ value: isEdit ? (existing.username || '') : '' });
  const passwordInput = inputEl({
    type: 'password',
    placeholder: isEdit ? 'leave blank to keep current password' : '',
  });
  const errEl = el('div', { class: 'banner error', style: 'display:none' });
  const submitBtn = el('button', { class: 'primary' }, isEdit ? 'Save' : 'Add');

  const close = () => { root.innerHTML = ''; };
  const showErr = (msg) => { errEl.textContent = msg; errEl.style.display = ''; };

  const submit = async () => {
    const payload = {
      name: nameInput.value.trim(),
      host: hostInput.value.trim(),
      port: parseInt(portInput.value || '0', 10),
      protocol: protocolSelect.value,
      username: usernameInput.value,
      password: passwordInput.value,
    };
    if (!payload.name) return showErr('Name is required');
    if (!payload.host) return showErr('Host is required');
    if (!payload.port) return showErr('Port is required');

    submitBtn.disabled = true; submitBtn.textContent = isEdit ? 'Saving…' : 'Adding…';
    errEl.style.display = 'none';
    try {
      if (isEdit) {
        await apiPATCH(`/api/v1/manual-proxies/${encodeURIComponent(existing.id)}`, payload);
      } else {
        await apiPOST('/api/v1/manual-proxies', payload);
      }
      close();
      await refreshAll();
    } catch (e) {
      if (e.status === 409 && e.data && e.data.error === 'manual_name_in_use') {
        showErr(`Name "${payload.name}" is already used by another manual proxy.`);
      } else {
        showErr(e.message);
      }
      submitBtn.disabled = false;
      submitBtn.textContent = isEdit ? 'Save' : 'Add';
    }
  };
  submitBtn.addEventListener('click', submit);

  root.appendChild(
    el('div', { class: 'modal-backdrop', onclick: (e) => { if (e.target.classList.contains('modal-backdrop')) close(); } },
      el('div', { class: 'modal' },
        el('h3', {}, isEdit ? 'Edit manual proxy' : 'Add manual proxy'),
        el('div', { class: 'field' }, el('label', {}, 'Name (unique)'), nameInput),
        el('div', { class: 'field' }, el('label', {}, 'Host'), hostInput),
        el('div', { class: 'row' },
          el('div', { class: 'field' }, el('label', {}, 'Port'), portInput),
          el('div', { class: 'field' }, el('label', {}, 'Protocol'), protocolSelect),
        ),
        el('div', { class: 'field' }, el('label', {}, 'Username (optional)'), usernameInput),
        el('div', { class: 'field' }, el('label', {}, 'Password (optional)'), passwordInput),
        errEl,
        el('div', { class: 'buttons' },
          el('button', { onclick: close }, 'Cancel'),
          submitBtn,
        ),
      ),
    ),
  );
}

function systemSection() {
  const s = state.settings || {
    sync_interval_minutes: 60,
    proxy_port: 8080,
    proxy_bind: '127.0.0.1',
    proxy_enabled: false,
    universal_proxy_password_set: false,
    subscription_enabled: false,
    subscription_host: '',
  };
  const section = el('section', { style: 'flex:1; min-width:320px' },
    el('h2', {},
      'System',
      el('span', { class: 'status-pill' },
        el('span', { class: 'status-dot ' + (state.proxy.running ? 'running' : 'stopped') }),
        ' ',
        state.proxy.running ? 'Running' : 'Stopped',
      ),
      el('span', { style: 'flex:1' }),
      el('button', {
        class: 'primary',
        onclick: () => state.proxy.running ? stopProxy() : startProxy(),
      }, state.proxy.running ? 'Stop proxy' : 'Start proxy'),
    ),
    card(
      el('div', { class: 'row' },
        field('Listen addr', selectEl(
          { onchange: (e) => { s.proxy_bind = e.target.value; } },
          [
            { value: '127.0.0.1', label: '127.0.0.1' },
            { value: '0.0.0.0', label: '0.0.0.0' },
            { value: '[::1]', label: '[::1]' },
          ],
          s.proxy_bind,
        )),
        field('Mixed Port', inputEl({
          type: 'number', value: s.proxy_port, style: 'width:90px',
          oninput: (e) => { s.proxy_port = parseInt(e.target.value || '0', 10); },
        })),
        field('Sync (min)', inputEl({
          type: 'number', value: s.sync_interval_minutes, style: 'width:80px',
          oninput: (e) => { s.sync_interval_minutes = parseInt(e.target.value || '0', 10); },
        })),
        el('div', { class: 'grow' }),
        el('button', { class: 'primary', onclick: () => applySettings(s) }, 'Apply'),
      ),
      el('div', { class: 'row' },
        el('div', { class: 'field' },
          el('label', {},
            'Universal password',
            infoTip('When set, a client can connect using a proxy’s display name as the username and this password to route through that proxy — no dedicated per-proxy user needed.'),
            el('span', { class: 'tag ' + (s.universal_proxy_password_set ? 'tag-on' : 'tag-off') },
              s.universal_proxy_password_set ? 'set' : 'not set'),
          ),
          inputEl({
            type: 'password',
            oninput: (e) => { s._universalPwd = e.target.value; },
          }),
        ),
        el('div', { class: 'grow' }),
        el('button', { class: 'primary', onclick: () => setUniversalPassword(s) }, 'Save'),
        s.universal_proxy_password_set
          ? el('button', { onclick: () => clearUniversalPassword() }, 'Clear')
          : null,
      ),
      state.listenerError ? el('div', { class: 'banner error' }, state.listenerError) : null,
    ),
  );
  return section;
}

function subscriptionSection() {
  const s = state.settings || {
    sync_interval_minutes: 60,
    proxy_port: 8080,
    proxy_bind: '127.0.0.1',
    proxy_enabled: false,
    universal_proxy_password_set: false,
    subscription_enabled: false,
    subscription_host: '',
  };
  const hasUniversal = s.universal_proxy_password_set;
  const enableTip = hasUniversal
    ? 'Serves a public /subscription endpoint listing your routable proxies for client apps; authenticated by the universal password.'
    : 'Set a Universal password in the System card first — the subscription endpoint authenticates with it.';
  return el('section', { style: 'flex:1; min-width:320px' },
    el('h2', {}, 'Subscription'),
    card(
      el('div', { class: 'row' },
        el('label', { class: 'checkbox-row' + (hasUniversal ? '' : ' disabled') },
          inputEl({
            type: 'checkbox',
            checked: s.subscription_enabled ? '' : null,
            disabled: hasUniversal ? null : '',
            onchange: (e) => { s.subscription_enabled = e.target.checked; },
          }),
          'Enable subscription',
          infoTip(enableTip),
        ),
      ),
      el('div', { class: 'row' },
        el('div', { class: 'field' },
          el('label', {},
            'Subscription host',
            infoTip('The public ip or domain clients use to reach the proxy. It fills the host:port of every generated subscription line.'),
          ),
          inputEl({
            value: s.subscription_host,
            placeholder: 'e.g. 192.168.2.241 or proxy.example.com',
            style: 'min-width:220px',
            oninput: (e) => { s.subscription_host = e.target.value; },
          }),
        ),
      ),
      el('div', { class: 'row' },
        el('button', { onclick: () => copySubscriptionURL() }, 'Copy subscription URL'),
        el('div', { class: 'grow' }),
        el('button', { class: 'primary', onclick: () => applySettings(s) }, 'Apply'),
      ),
    ),
  );
}

async function copySubscriptionURL() {
  try {
    const r = await apiGET('/api/v1/subscription-url');
    if (!r || !r.enabled || !r.url) {
      alert('Subscription is not active. Enable it and set a universal password first, then Apply.');
      return;
    }
    await copyText(r.url);
  } catch (e) { alert('Copy failed: ' + e.message); }
}

async function copyText(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch (_) {}
  }
  // Fallback for plain-http LAN contexts where async clipboard API is blocked.
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.focus();
  ta.select();
  document.execCommand('copy');
  document.body.removeChild(ta);
}

async function testLatency() {
  const btns = document.querySelectorAll('.test-latency-btn');
  btns.forEach(b => { b.disabled = true; b.textContent = 'Testing…'; });
  try { await apiPOST('/api/v1/upstreams/test-latency'); }
  catch (e) { alert('Latency test failed: ' + e.message); }
  await refreshAll();
}

function latencyCell(ms) {
  if (ms === undefined || ms === null) return el('span', { class: 'muted' }, '—');
  if (ms < 0) return el('span', { class: 'lat-bad' }, 'failed');
  const cls = ms < 300 ? 'lat-good' : ms < 800 ? 'lat-ok' : 'lat-bad';
  return el('span', { class: cls }, ms + ' ms');
}

function webshareSection() {
  return el('section', {},
    el('h2', {},
      'Webshare',
      el('span', { style: 'flex:1' }),
      el('button', { class: 'test-latency-btn icon', title: 'Test latency for all proxies', onclick: () => testLatency() }, '⏱'),
      el('button', { class: 'icon', title: 'Add API key', onclick: () => openAddKeyModal() }, '+'),
    ),
    state.keys.length === 0
      ? el('div', { class: 'card empty' }, 'No API keys configured. Click + to add one.')
      : el('div', {}, ...state.keys.map(renderKeyCard)),
  );
}

function renderKeyCard(key) {
  // Dead (alive=false) webshare upstreams are pruned by sync; hide any that
  // linger in the client snapshot between a row going stale and the next sync.
  const owned = state.upstreams.filter((u) => u.source_api_key_id === key.id && u.alive);
  return el('div', { class: 'key-card' },
    el('div', { class: 'header' },
      el('span', { class: 'label' }, key.label),
      key.last_sync_error
        ? el('span', { class: 'err-dot', title: key.last_sync_error }, '●')
        : null,
      key.last_synced_at
        ? el('span', { class: 'timestamp' }, formatRelative(key.last_synced_at))
        : el('span', { class: 'timestamp' }, 'never synced'),
      el('button', { class: 'icon', title: 'Sync', onclick: () => syncKey(key.id) }, '↻'),
      el('button', { class: 'icon danger-icon', title: 'Delete', onclick: () => deleteKey(key.id) }, '✕'),
    ),
    owned.length === 0
      ? el('div', { class: 'empty', style: 'padding:8px' }, 'No upstreams synced yet')
      : renderUpstreams(owned),
  );
}

function renderUpstreams(rows) {
  return el('div', { class: 'upstreams' },
    el('div', { class: 'upstream-row head' },
      el('div', {}, 'Country'),
      el('div', {}, 'Display Name'),
      el('div', { class: 'col-host' }, 'Node Address'),
      el('div', {}, 'Alive'),
      el('div', {}, 'Latency'),
      el('div', {}, 'Actions'),
    ),
    ...rows.map((u) =>
      el('div', { class: 'upstream-row' },
        el('div', {}, u.country_code || '—'),
        el('div', {}, u.display_name),
        el('div', { class: 'mono col-host' }, `${u.host}:${u.port}`),
        u.alive
          ? el('div', { class: 'alive-yes' }, '✓')
          : el('div', { class: 'alive-no' }, '✗'),
        el('div', {}, latencyCell(u.last_latency_ms)),
        el('div', {},
          el('button', { class: 'icon', title: 'Replace proxy', onclick: () => openReplaceProxyModal(u) }, '⇄'),
        ),
      ),
    ),
  );
}

function openReplaceProxyModal(u) {
  const root = document.getElementById('modal-root');
  root.innerHTML = '';

  const resultEl = el('div', { class: 'banner', style: 'display:none' });
  const proxySelectEl = el('select', {});
  proxySelectEl.style.width = '100%';

  const radioCountry = el('input', { type: 'radio', name: 'replacewith', value: 'country' });
  radioCountry.checked = true;
  const radioAsn = el('input', { type: 'radio', name: 'replacewith', value: 'asn' });

  const previewBtn = el('button', {}, 'Preview');
  const replaceBtn = el('button', { class: 'primary' }, 'Replace');

  const close = () => { root.innerHTML = ''; };

  const showResult = (msg, isError) => {
    resultEl.textContent = msg;
    resultEl.className = 'banner ' + (isError ? 'error' : 'note');
    resultEl.style.display = '';
  };

  const setLoading = (loading) => {
    previewBtn.disabled = loading;
    replaceBtn.disabled = loading;
  };

  let opts = null;

  const fillSelect = (mode) => {
    proxySelectEl.innerHTML = '';
    if (!opts) return;
    if (mode === 'country') {
      for (const c of opts.countries) {
        const o = document.createElement('option');
        o.value = c.code;
        o.textContent = `${c.code} — ${c.available} available`;
        if (c.code === u.country_code) o.selected = true;
        proxySelectEl.appendChild(o);
      }
    } else {
      for (const a of opts.asns) {
        const o = document.createElement('option');
        o.value = a.number;
        o.textContent = `${a.name} (${a.number}) — ${a.available} available`;
        proxySelectEl.appendChild(o);
      }
    }
  };

  const getMode = () => radioCountry.checked ? 'country' : 'asn';

  const buildBody = (dryRun) => {
    const mode = getMode();
    if (mode === 'country') {
      return { replace_with: 'country', country_code: proxySelectEl.value, dry_run: dryRun };
    }
    return { replace_with: 'asn', asn_numbers: [parseInt(proxySelectEl.value, 10)], dry_run: dryRun };
  };

  const doReplace = async (dryRun) => {
    setLoading(true);
    resultEl.style.display = 'none';
    try {
      const r = await apiPOST(`/api/v1/upstreams/${encodeURIComponent(u.id)}/replace`, buildBody(dryRun));
      if (dryRun) {
        showResult(`Would remove ${r.proxies_removed}, add ${r.proxies_added}`, false);
        setLoading(false);
      } else {
        close();
        await refreshAll();
      }
    } catch (e) {
      const msg = (e.data && e.data.error) ? e.data.error : e.message;
      showResult(msg, true);
      setLoading(false);
    }
  };

  radioCountry.addEventListener('change', () => fillSelect('country'));
  radioAsn.addEventListener('change', () => fillSelect('asn'));
  previewBtn.addEventListener('click', () => doReplace(true));
  replaceBtn.addEventListener('click', () => doReplace(false));

  const modal = el('div', { class: 'modal' },
    el('h3', {}, `Replace ${u.display_name}`),
    el('div', { class: 'field' },
      el('label', {}, 'Current proxy'),
      el('div', {},
        el('span', { class: 'mono' }, `${u.host}:${u.port}`),
        ' ',
        el('span', { class: 'muted' }, u.country_code || '—'),
      ),
    ),
    el('div', { class: 'field' },
      el('label', {}, 'Replace with'),
      el('div', { class: 'row', style: 'gap:16px' },
        el('label', { class: 'checkbox-row' }, radioCountry, 'Country'),
        el('label', { class: 'checkbox-row' }, radioAsn, 'ASN'),
      ),
    ),
    el('div', { class: 'field' },
      el('label', {}, 'Selection'),
      proxySelectEl,
    ),
    resultEl,
    el('div', { class: 'buttons' },
      el('button', { onclick: close }, 'Cancel'),
      previewBtn,
      replaceBtn,
    ),
  );

  setLoading(true);
  proxySelectEl.disabled = true;

  root.appendChild(
    el('div', { class: 'modal-backdrop', onclick: (e) => { if (e.target.classList.contains('modal-backdrop')) close(); } },
      modal,
    ),
  );

  apiGET('/api/v1/keys/' + u.source_api_key_id + '/replace-options')
    .then((data) => {
      opts = data;
      proxySelectEl.disabled = false;
      fillSelect(getMode());
      setLoading(false);
    })
    .catch((e) => {
      const msg = (e.data && e.data.error) ? e.data.error : e.message;
      showResult('Failed to load options: ' + msg, true);
      proxySelectEl.disabled = true;
    });
}

function renderUsers() {
  $app.innerHTML = '';
  const section = el('section', {},
    el('h2', {},
      'Users',
      el('span', { style: 'flex:1' }),
      el('button', { class: 'icon', title: 'Add user', onclick: () => openAddUserModal() }, '+'),
    ),
    state.users.length === 0
      ? el('div', { class: 'card empty' }, 'No users yet. Click + to add one.')
      : el('div', { class: 'card', style: 'padding:0' }, renderUsersTable()),
  );
  $app.appendChild(section);
}

function renderUsersTable() {
  const upstreamOptions = [{ value: '', label: '— (unmapped)' }]
    .concat(state.upstreams.map((u) => ({
      value: u.id,
      label: u.source === 'manual' ? `${u.display_name} (manual)` : u.display_name,
    })));

  const tbody = el('tbody', {}, ...state.users.map((user, idx) => {
    const revealed = state.revealedPasswords[user.username];
    return el('tr', {},
      el('td', {}, user.username),
      el('td', {},
        selectEl(
          { onchange: (e) => setMapping(user.username, e.target.value || null) },
          upstreamOptions,
          user.upstream_proxy_id || '',
        ),
      ),
      el('td', {},
        el('div', { class: 'row', style: 'gap:6px' },
          revealed ? el('span', { class: 'mono' }, revealed.value) : el('span', { class: 'muted' }, '••••••'),
          el('button', {
            class: 'icon', title: revealed ? 'Hide' : 'Reveal',
            onclick: () => peekPassword(user.username),
          }, revealed ? '🙈' : '👁'),
        ),
      ),
      el('td', {},
        user.broken
          ? el('span', { class: 'broken', title: 'Mapping broken — upstream missing or stale' }, '⚠')
          : el('span', { class: 'ok' }, '✓'),
      ),
      el('td', { class: 'actions' },
        el('div', { class: 'action-group' },
          el('button', {
            class: 'icon', title: 'Move up',
            disabled: idx === 0 ? '' : null,
            onclick: () => moveUser(idx, -1),
          }, '↑'),
          el('button', {
            class: 'icon', title: 'Move down',
            disabled: idx === state.users.length - 1 ? '' : null,
            onclick: () => moveUser(idx, +1),
          }, '↓'),
          el('button', {
            class: 'icon danger-icon', title: 'Delete',
            onclick: () => deleteUser(user.username),
          }, '✕'),
        ),
      ),
    );
  }));

  return el('table', { class: 'user-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Username'),
      el('th', {}, 'Mapped Proxy'),
      el('th', {}, 'Password'),
      el('th', {}, 'Status'),
      el('th', { class: 'actions' }, 'Actions'),
    )),
    tbody,
  );
}

// --- Actions ---

async function applySettings(s) {
  try {
    state.listenerError = '';
    await apiPUT('/api/v1/settings', {
      sync_interval_minutes: s.sync_interval_minutes,
      proxy_port: s.proxy_port,
      proxy_bind: s.proxy_bind,
      subscription_enabled: s.subscription_enabled,
      subscription_host: s.subscription_host,
    });
  } catch (e) {
    state.listenerError = e.message;
  }
  await refreshAll();
}

async function setUniversalPassword(s) {
  if (!s._universalPwd) {
    state.listenerError = 'Enter a password to set, or use Clear to remove it.';
    render();
    return;
  }
  try {
    await apiPUT('/api/v1/settings/universal-password', { password: s._universalPwd });
  } catch (e) {
    state.listenerError = e.message;
  }
  await refreshAll();
}

async function clearUniversalPassword() {
  if (!confirm('Clear the universal proxy password? Clients using a display name + this password will stop working.')) return;
  try {
    await apiPUT('/api/v1/settings/universal-password', { password: '' });
  } catch (e) {
    state.listenerError = e.message;
  }
  await refreshAll();
}

async function startProxy() {
  try {
    state.listenerError = '';
    await apiPOST('/api/v1/proxy/start');
  } catch (e) {
    state.listenerError = e.message;
  }
  await refreshAll();
}

async function stopProxy() {
  try {
    state.listenerError = '';
    await apiPOST('/api/v1/proxy/stop');
  } catch (e) {
    state.listenerError = e.message;
  }
  await refreshAll();
}

async function syncKey(id) {
  try { await apiPOST(`/api/v1/keys/${id}/sync`); }
  catch (e) { alert('Sync failed: ' + e.message); }
  await refreshAll();
}

async function deleteKey(id) {
  if (!confirm('Delete this API key? Synced upstreams will be removed.')) return;
  try { await apiDELETE(`/api/v1/keys/${id}`); }
  catch (e) {
    if (e.status === 409 && e.data && e.data.referencing_users) {
      const lines = e.data.referencing_users
        .map((r) => `• ${r.username} → ${r.display_name}`).join('\n');
      alert('Key is in use by:\n' + lines);
    } else {
      alert('Delete failed: ' + e.message);
    }
  }
  await refreshAll();
}

async function setMapping(username, upstreamId) {
  try { await apiPATCH(`/api/v1/users/${encodeURIComponent(username)}`, { upstream_proxy_id: upstreamId }); }
  catch (e) { alert('Set mapping failed: ' + e.message); }
  await refreshAll();
}

async function peekPassword(username) {
  if (state.revealedPasswords[username]) {
    clearTimeout(state.revealedPasswords[username].timerId);
    delete state.revealedPasswords[username];
    render();
    return;
  }
  try {
    const r = await apiGET(`/api/v1/users/${encodeURIComponent(username)}/password`);
    const timerId = setTimeout(() => {
      delete state.revealedPasswords[username];
      render();
    }, 5000);
    state.revealedPasswords[username] = { value: r.password, timerId };
    render();
  } catch (e) {
    alert('Peek failed: ' + e.message);
  }
}

async function deleteUser(username) {
  if (!confirm(`Delete user "${username}"?`)) return;
  try { await apiDELETE(`/api/v1/users/${encodeURIComponent(username)}`); }
  catch (e) { alert('Delete failed: ' + e.message); }
  await refreshAll();
}

async function moveUser(idx, delta) {
  const j = idx + delta;
  if (j < 0 || j >= state.users.length) return;
  const next = state.users.slice();
  [next[idx], next[j]] = [next[j], next[idx]];
  const usernames = next.map((u) => u.username);
  try { await apiPOST('/api/v1/users/reorder', usernames); }
  catch (e) { alert('Reorder failed: ' + e.message); }
  await refreshAll();
}

// --- Modals ---

function openAddKeyModal() {
  const root = document.getElementById('modal-root');
  root.innerHTML = '';

  const labelInput = inputEl({ autofocus: '' });
  const keyInput = inputEl({ type: 'password' });
  const errEl = el('div', { class: 'banner error', style: 'display:none' });
  const submitBtn = el('button', { class: 'primary' }, 'Add');

  const close = () => { root.innerHTML = ''; };
  const submit = async () => {
    if (!labelInput.value || !keyInput.value) {
      errEl.textContent = 'Label and key are required'; errEl.style.display = '';
      return;
    }
    submitBtn.disabled = true; submitBtn.textContent = 'Adding…';
    errEl.style.display = 'none';
    try {
      await apiPOST('/api/v1/keys', { Label: labelInput.value, APIKey: keyInput.value });
      close();
      await refreshAll();
    } catch (e) {
      errEl.textContent = e.message; errEl.style.display = '';
      submitBtn.disabled = false; submitBtn.textContent = 'Add';
    }
  };
  submitBtn.addEventListener('click', submit);

  root.appendChild(
    el('div', { class: 'modal-backdrop', onclick: (e) => { if (e.target.classList.contains('modal-backdrop')) close(); } },
      el('div', { class: 'modal' },
        el('h3', {}, 'Add API key'),
        el('div', { class: 'field' }, el('label', {}, 'Label'), labelInput),
        el('div', { class: 'field' }, el('label', {}, 'API key (sk_…)'), keyInput),
        errEl,
        el('div', { class: 'buttons' },
          el('button', { onclick: close }, 'Cancel'),
          submitBtn,
        ),
      ),
    ),
  );
}

function openAddUserModal() {
  const root = document.getElementById('modal-root');
  root.innerHTML = '';

  const usernameInput = inputEl({ autofocus: '' });
  const passwordInput = inputEl({ type: 'password' });
  const upstreamOptions = [{ value: '', label: '— (no mapping)' }]
    .concat(state.upstreams.map((u) => ({
      value: u.id,
      label: u.source === 'manual' ? `${u.display_name} (manual)` : u.display_name,
    })));
  const upstreamSelect = selectEl({}, upstreamOptions, '');
  const errEl = el('div', { class: 'banner error', style: 'display:none' });
  const submitBtn = el('button', { class: 'primary' }, 'Add');

  const close = () => { root.innerHTML = ''; };
  const submit = async () => {
    const username = usernameInput.value;
    const password = passwordInput.value;
    if (!username || !password) {
      errEl.textContent = 'Username and password are required'; errEl.style.display = '';
      return;
    }
    submitBtn.disabled = true; submitBtn.textContent = 'Adding…';
    errEl.style.display = 'none';
    try {
      await apiPOST('/api/v1/users', { Username: username, Password: password });
      if (upstreamSelect.value) {
        await apiPATCH(`/api/v1/users/${encodeURIComponent(username)}`, { upstream_proxy_id: upstreamSelect.value });
      }
      close();
      await refreshAll();
    } catch (e) {
      errEl.textContent = e.message; errEl.style.display = '';
      submitBtn.disabled = false; submitBtn.textContent = 'Add';
    }
  };
  submitBtn.addEventListener('click', submit);

  root.appendChild(
    el('div', { class: 'modal-backdrop', onclick: (e) => { if (e.target.classList.contains('modal-backdrop')) close(); } },
      el('div', { class: 'modal' },
        el('h3', {}, 'Add user'),
        el('div', { class: 'field' }, el('label', {}, 'Username'), usernameInput),
        el('div', { class: 'field' }, el('label', {}, 'Password'), passwordInput),
        el('div', { class: 'field' }, el('label', {}, 'Mapped Proxy'), upstreamSelect),
        errEl,
        el('div', { class: 'buttons' },
          el('button', { onclick: close }, 'Cancel'),
          submitBtn,
        ),
      ),
    ),
  );
}

// --- DOM helpers ---

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined) continue;
      if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
      else if (k === 'class') node.className = v;
      else if (k === 'disabled' || k === 'autofocus' || k === 'checked' || k === 'readonly') {
        if (v !== null && v !== false) node.setAttribute(k, '');
      } else node.setAttribute(k, v);
    }
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.appendChild(typeof c === 'string' || typeof c === 'number' ? document.createTextNode(String(c)) : c);
  }
  return node;
}

function card(...children) {
  return el('div', { class: 'card' }, ...children);
}

function field(label, input) {
  return el('div', { class: 'field' }, el('label', {}, label), input);
}

// infoTip renders a small ⓘ icon that reveals a help bubble on hover or
// click. Used next to field labels so explanatory text doesn't have to live
// in placeholders.
function infoTip(text) {
  return el('span', {
    class: 'info-tip',
    tabindex: '0',
    role: 'button',
    'aria-label': text,
    onclick: (e) => { e.preventDefault(); e.stopPropagation(); e.currentTarget.classList.toggle('open'); },
  }, 'ⓘ', el('span', { class: 'info-bubble' }, text));
}

function inputEl(attrs) {
  return el('input', attrs);
}

function selectEl(attrs, options, currentValue) {
  const select = el('select', attrs);
  for (const opt of options) {
    const o = document.createElement('option');
    o.value = opt.value;
    o.textContent = opt.label;
    if (String(opt.value) === String(currentValue)) o.selected = true;
    select.appendChild(o);
  }
  return select;
}

function formatRelative(iso) {
  const t = new Date(iso).getTime();
  const diff = Date.now() - t;
  const s = Math.floor(diff / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return new Date(iso).toLocaleString();
}

// --- Boot ---

document.getElementById('tabs').addEventListener('click', (e) => {
  const t = e.target.closest('.tab');
  if (!t) return;
  state.tab = t.dataset.tab;
  try { localStorage.setItem(TAB_STORAGE_KEY, state.tab); } catch (_) {}
  render();
});

document.getElementById('refresh-btn').addEventListener('click', () => refreshAll());

document.getElementById('logout-btn').addEventListener('click', async () => {
  try { await apiPOST('/web/api/logout'); } catch (_) {}
  location.href = '/login';
});

refreshAll();
setInterval(refreshAll, 30000);
