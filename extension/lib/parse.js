// Pure subscription-line parsing for the PIA proxy extension.
//
// The PIA daemon serves `GET /subscription?...` as text/plain, one proxy per
// line, in the form:
//
//   <scheme>://<display-name>:<universal-password>@<host>:<port>#<display-name>
//
// where <scheme> is `socks` (default) or `http` (?type=http). This module turns
// that text into structured proxy records. It has NO chrome.* dependencies so
// it can be unit-tested under plain Node.

// One subscription line. Userinfo is percent-encoded by the daemon (Go's
// url.UserPassword), so `@` never appears unescaped inside username/password —
// which lets us split the authority unambiguously.
const LINE_RE =
  /^([a-zA-Z][\w+.\-]*):\/\/(?:([^:@\s]+)(?::([^@\s]*))?@)?([^:@/#?\s]+):(\d+)(?:#(.*))?$/;

// safeDecode percent-decodes a userinfo/fragment component, returning the raw
// input unchanged if it is not valid percent-encoding (so a stray `%` never
// throws away an otherwise-usable proxy line).
function safeDecode(s) {
  if (s == null) return '';
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

// chromeScheme maps a subscription URI scheme to a chrome.proxy `singleProxy`
// scheme. SOCKS variants collapse to socks5 (PIA's unified port speaks SOCKS5).
export function chromeScheme(rawScheme) {
  switch (rawScheme.toLowerCase()) {
    case 'socks':
    case 'socks5':
      return 'socks5';
    case 'socks4':
      return 'socks4';
    case 'https':
      return 'https';
    case 'http':
    default:
      return 'http';
  }
}

// parseLine parses a single subscription line into a proxy record, or returns
// null if the line is blank, a comment, or malformed.
export function parseLine(line) {
  const trimmed = line.trim();
  if (trimmed === '' || trimmed.startsWith('#') || trimmed.startsWith('//')) {
    return null;
  }
  const m = LINE_RE.exec(trimmed);
  if (!m) return null;

  const [, rawScheme, rawUser, rawPass, host, portStr, rawFragment] = m;
  const port = Number(portStr);
  if (!Number.isInteger(port) || port < 1 || port > 65535) return null;

  const username = safeDecode(rawUser);
  const password = safeDecode(rawPass);
  const name = safeDecode(rawFragment) || username || `${host}:${port}`;

  return {
    raw: trimmed,
    rawScheme: rawScheme.toLowerCase(),
    scheme: chromeScheme(rawScheme),
    host,
    port,
    username,
    password,
    name,
  };
}

// parseSubscription parses a full subscription body into an array of proxy
// records, skipping blank/comment/malformed lines. A stable `id` is assigned
// per record from its routing identity so selections survive a re-fetch.
export function parseSubscription(text) {
  const out = [];
  const seen = new Set();
  for (const line of String(text).split(/\r?\n/)) {
    const proxy = parseLine(line);
    if (!proxy) continue;
    const id = `${proxy.scheme}://${proxy.username}@${proxy.host}:${proxy.port}`;
    if (seen.has(id)) continue;
    seen.add(id);
    out.push({ ...proxy, id });
  }
  return out;
}

// withType returns the subscription URL with the `type` query parameter forced
// to the given value (default "http", which the extension needs so Chrome can
// authenticate the proxy via onAuthRequired). Throws on an unparseable URL.
export function withType(rawUrl, type = 'http') {
  const u = new URL(rawUrl);
  u.searchParams.set('type', type);
  return u.toString();
}
