/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import assert from 'node:assert/strict';
import test from 'node:test';

import { ALLOWLIST, check, extractKeys, MIN_KEYS, REQUIRED_KEYS } from './settings-coverage.mjs';

/** Keys that satisfy the lower bound. */
function enoughKeys() {
  return [...REQUIRED_KEYS, ...Array.from({ length: MIN_KEYS }, (_, i) => `filler${i}`)];
}

/** @param {string} route @param {Partial<{ file: string, skipped: boolean }>} [extra] */
function decl(route, extra = {}) {
  return { file: 'tests/playwright/specs/a.spec.ts', line: 1, title: 't', route, skipped: false, ...extra };
}

const TRACKED = new Set(['tests/playwright/specs/a.spec.ts']);

/** Declares every key of `keys` except `except`. */
function declareAll(keys, except = []) {
  return keys.filter((k) => !except.includes(k)).map((k) => decl(k));
}

test('extractKeys reads prefer.model calls in script setup, not elsewhere', () => {
  const source = `<template><div>{{ prefer.model('inTemplate') }}</div></template>
<script lang="ts" setup>
const a = prefer.model('squareAvatars');
const b = prefer.model("deck.columnAlign", (v) => !v, (v) => !v);
// prefer.model('commentedOut')
const c = other.model('notPrefer');
const d = prefer.r.notModel;
</script>`;
  assert.deepEqual(extractKeys(source), ['squareAvatars', 'deck.columnAlign']);
});

test('every key declared passes', () => {
  const keys = enoughKeys();
  const result = check({ keys, declarations: declareAll(keys), tracked: TRACKED, allowlist: '' });
  assert.deepEqual(result.problems, []);
  assert.deepEqual(result.untrusted, []);
});

test('a key with no declaration and no allowlist row is a problem', () => {
  const keys = enoughKeys();
  const result = check({ keys, declarations: declareAll(keys, ['filler3']), tracked: TRACKED, allowlist: '' });
  assert.equal(result.problems.length, 1);
  assert.match(result.problems[0], /^filler3: どの spec の行も確かめていない/);
});

test('an allowlisted key passes, and a stale or covered allowlist row is a problem', () => {
  const keys = enoughKeys();
  const ok = check({ keys, declarations: declareAll(keys, ['filler3']), tracked: TRACKED, allowlist: 'filler3\treason\n' });
  assert.deepEqual(ok.problems, []);
  assert.equal(ok.allowed, 1);
  const stale = check({ keys, declarations: declareAll(keys), tracked: TRACKED, allowlist: 'gone\treason\nfiller3\treason\n' });
  assert.equal(stale.problems.length, 2);
  assert.match(stale.problems.join('\n'), new RegExp(`${ALLOWLIST}:1: 設定のページに無いキー`));
  assert.match(stale.problems.join('\n'), /filler3 は spec が宣言している/);
});

test('declarations of an unknown key, an untracked file or a skipped test do not count', () => {
  const keys = enoughKeys();
  const declarations = [
    ...declareAll(keys, ['filler1', 'filler2']),
    decl('noSuchKey'),
    decl('filler1', { file: 'tests/playwright/specs/untracked.spec.ts' }),
    decl('filler2', { skipped: true }),
  ];
  const result = check({ keys, declarations, tracked: TRACKED, allowlist: '' });
  const text = result.problems.join('\n');
  assert.match(text, /設定のページに無いキー \(消すか直す\): noSuchKey/);
  assert.match(text, /git が追跡していない spec の宣言は数えない/);
  assert.match(text, /skip \/ fixme のテストの宣言は数えない/);
  assert.match(text, /^filler1: どの spec の行も確かめていない/m);
  assert.match(text, /^filler2: どの spec の行も確かめていない/m);
});

test('too few keys, a missing marker, no declarations or a malformed allowlist cannot be trusted', () => {
  const few = check({ keys: [...REQUIRED_KEYS], declarations: declareAll([...REQUIRED_KEYS]), tracked: TRACKED, allowlist: '' });
  assert.match(few.untrusted.join('\n'), /キーが \d+ 個しか拾えなかった/);
  const keys = enoughKeys().filter((k) => k !== REQUIRED_KEYS[0]);
  const marker = check({ keys, declarations: declareAll(keys), tracked: TRACKED, allowlist: '' });
  assert.deepEqual(marker.untrusted, [`目印のキー ${REQUIRED_KEYS[0]} が拾えなかった`]);
  const none = check({ keys: enoughKeys(), declarations: [], tracked: TRACKED, allowlist: '' });
  assert.match(none.untrusted.join('\n'), /spec の宣言を 1 件も拾えなかった/);
  const all = enoughKeys();
  const bad = check({ keys: all, declarations: declareAll(all), tracked: TRACKED, allowlist: 'no-tab-here\n' });
  assert.match(bad.untrusted.join('\n'), /2 列になっていない/);
});
