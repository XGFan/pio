// Unit tests for the subscription line parser. Run with: node --test
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  parseLine,
  parseSubscription,
  chromeScheme,
  withType,
} from '../lib/parse.js';

test('parses a socks line into a socks5 proxy record', () => {
  const p = parseLine('socks://US-A-01:masterpw@proxy.example.com:8080#US-A-01');
  assert.equal(p.scheme, 'socks5');
  assert.equal(p.rawScheme, 'socks');
  assert.equal(p.host, 'proxy.example.com');
  assert.equal(p.port, 8080);
  assert.equal(p.username, 'US-A-01');
  assert.equal(p.password, 'masterpw');
  assert.equal(p.name, 'US-A-01');
});

test('parses an http line into an http proxy record', () => {
  const p = parseLine('http://US-A-01:masterpw@proxy.example.com:8080#US-A-01');
  assert.equal(p.scheme, 'http');
  assert.equal(p.rawScheme, 'http');
  assert.equal(p.port, 8080);
});

test('socks5 scheme also normalizes to socks5', () => {
  assert.equal(chromeScheme('socks5'), 'socks5');
  assert.equal(chromeScheme('SOCKS'), 'socks5');
  assert.equal(chromeScheme('http'), 'http');
  assert.equal(chromeScheme('https'), 'https');
});

test('percent-encoded password is decoded', () => {
  const p = parseLine('http://user:p%40ss%3Aword@h.example:3128#name');
  assert.equal(p.password, 'p@ss:word');
  assert.equal(p.username, 'user');
});

test('fragment is used as the display name; falls back to username then host:port', () => {
  assert.equal(parseLine('http://u:p@h:80#Pretty Name').name, 'Pretty Name');
  assert.equal(parseLine('http://u:p@h:80').name, 'u');
  assert.equal(parseLine('socks://h.example:1080').name, 'h.example:1080');
});

test('rejects blank, comment, and malformed lines', () => {
  assert.equal(parseLine(''), null);
  assert.equal(parseLine('   '), null);
  assert.equal(parseLine('# a comment'), null);
  assert.equal(parseLine('// also a comment'), null);
  assert.equal(parseLine('not a url'), null);
  assert.equal(parseLine('http://host-without-port#x'), null);
  assert.equal(parseLine('http://h:0#x'), null); // port out of range
  assert.equal(parseLine('http://h:70000#x'), null);
});

test('parseSubscription skips junk, dedupes, and assigns stable ids', () => {
  const body = [
    '# PIA subscription',
    'http://US-A-01:pw@p.example:8080#US-A-01',
    '',
    'http://US-B-02:pw@p.example:8080#US-B-02',
    'http://US-A-01:pw@p.example:8080#US-A-01', // duplicate
    'garbage line',
  ].join('\n');
  const proxies = parseSubscription(body);
  assert.equal(proxies.length, 2);
  assert.equal(proxies[0].name, 'US-A-01');
  assert.equal(proxies[1].name, 'US-B-02');
  assert.equal(proxies[0].id, 'http://US-A-01@p.example:8080');
  assert.notEqual(proxies[0].id, proxies[1].id);
});

test('parseSubscription handles CRLF line endings', () => {
  const proxies = parseSubscription(
    'http://a:b@h:80#A\r\nhttp://c:d@h:81#B\r\n',
  );
  assert.equal(proxies.length, 2);
});

test('withType forces the type query parameter', () => {
  assert.equal(
    withType('https://panel.example/subscription?password=secret'),
    'https://panel.example/subscription?password=secret&type=http',
  );
  // Overrides an existing type and preserves other params.
  assert.equal(
    withType('https://panel.example/subscription?password=s&type=socks', 'http'),
    'https://panel.example/subscription?password=s&type=http',
  );
});

test('withType throws on an invalid url', () => {
  assert.throws(() => withType('not a url'));
});
