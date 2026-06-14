// Unit tests for badgeAbbrev. Run with: node extension/test/badge.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { badgeAbbrev } from '../lib/badge.js';

test('extracts country code from canonical share-CC-NN name', () => {
  assert.equal(badgeAbbrev('share-DE-01'), 'DE');
});

test('extracts country code from canonical dedicated-CC-NN name', () => {
  assert.equal(badgeAbbrev('dedicated-US-01'), 'US');
});

test('no-dash name: compact-first-3 abbreviation', () => {
  assert.equal(badgeAbbrev('WH'), 'WH');
  assert.equal(badgeAbbrev('default'), 'DEF');
});

test('empty segment (double-dash) falls back to compact-first-3', () => {
  // "a--b": split('-') → ['a','','b'], segment[1] is '', so fall back
  assert.equal(badgeAbbrev('a--b'), 'AB');
  // "share--01": segment[1] is '', so fall back to compact of whole name
  assert.equal(badgeAbbrev('share--01'), 'SHA');
});

test('badge is never blank', () => {
  // name with no letters/digits at all → 'ON'
  assert.equal(badgeAbbrev('---'), 'ON');
  assert.equal(badgeAbbrev(''), 'ON');
  assert.equal(badgeAbbrev(null), 'ON');
});

test('country segment is uppercased and capped at 4 chars', () => {
  assert.equal(badgeAbbrev('vpn-usdallas-01'), 'USDA');
});
