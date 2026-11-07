/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import { checkKind, definitionKeys, extractMetaKeys, extractPolicyKeys, splitAllowlist } from './admin-settings-coverage.mjs';

const here = fileURLToPath(new URL('.', import.meta.url));
const sfc = createRequire(join(here, '..', '..', 'frontend/packages/frontend/package.json'))('vue/compiler-sfc');
const parseTemplate = (/** @type {string} */ src) => sfc.parse(src).descriptor.template.ast;

/** @param {string} route @param {Partial<{ file: string, skipped: boolean }>} [extra] */
function decl(route, extra = {}) {
  return { file: 'tests/playwright/specs/a.spec.ts', line: 1, title: 't', route, skipped: false, ...extra };
}
const TRACKED = new Set(['tests/playwright/specs/a.spec.ts']);

test('extractMetaKeys follows literals, spreads and conditionals, and resolves named arguments', () => {
  const source = `<script lang="ts" setup>
os.apiWithDialog('admin/update-meta', { name: v, 'themeColor': c, ...(ok ? { chunkedUploadEnabled: a } : {}) });
misskeyApi('admin/update-meta', registrationModePatch[mode]);
misskeyApi('admin/update-meta', somethingElse);
misskeyApi('admin/other', { notMeta: 1 });
// misskeyApi('admin/update-meta', { commentedOut: 1 });
</script>`;
  const r = extractMetaKeys(source, true, (name) => (name === 'registrationModePatch' ? ['disableRegistration'] : null));
  assert.deepEqual(r.keys, ['name', 'themeColor', 'chunkedUploadEnabled', 'disableRegistration']);
  assert.deepEqual(r.unresolved, ['somethingElse']);
});

test('definitionKeys collects the keys of the object literals inside a declaration', () => {
  const source = `export const registrationModePatch = {
  open: { disableRegistration: false, approvalRequiredForSignup: false },
  closed: { registrationClosed: true },
} as const;
const other = { notThis: 1 };`;
  assert.deepEqual(definitionKeys(source, 'registrationModePatch'), ['disableRegistration', 'approvalRequiredForSignup', 'registrationClosed']);
  assert.equal(definitionKeys(source, 'missing'), null);
});

test('extractPolicyKeys reads data-policy-key and checks it against the bound policy', () => {
  const source = `<template><div>
<XFolder v-if="a" v-model:policyMeta="policyMetaModel.gtlAvailable" data-policy-key="gtlAvailable"></XFolder>
<XFolder v-model:policyMeta="canSearchIpHistoryMeta" data-policy-key="canSearchIpHistory"></XFolder>
<XFolder v-model:policyMeta="optOutPolicyMeta" data-policy-key="optOutNotificationTypes"></XFolder>
<XFolder v-model:policyMeta="policyMetaModel.canImportUserLists" data-policy-key="canImportUserList"></XFolder>
<XFolder v-model:policyMeta="policyMetaModel.ltlAvailable"></XFolder>
</div></template>`;
  const r = extractPolicyKeys(source, parseTemplate);
  assert.deepEqual(r.keys, ['gtlAvailable', 'canSearchIpHistory', 'optOutNotificationTypes', 'canImportUserList']);
  assert.equal(r.problems.length, 2);
  assert.match(r.problems[0], /data-policy-key="canImportUserList" が、編集するポリシー \(policyMetaModel\.canImportUserLists\) と合わない/);
  assert.match(r.problems[1], /data-policy-key が無い/);
});

/** @param {Partial<Parameters<typeof checkKind>[0]>} over */
function run(over) {
  return checkKind({ kind: 'meta', keys: ['a', 'b', 'c'], declarations: [decl('a'), decl('b'), decl('c')], tracked: TRACKED, allowRows: [], minKeys: 3, requiredKeys: ['a'], where: '画面', ...over });
}

test('every key declared passes', () => {
  const r = run({});
  assert.deepEqual(r.problems, []);
  assert.deepEqual(r.untrusted, []);
  assert.equal(r.covered, 3);
});

test('a key with no declaration and no allowlist row is a problem', () => {
  const r = run({ declarations: [decl('a'), decl('b')] });
  assert.deepEqual(r.problems, ['meta:c: どの spec の行も確かめていない (fixtures/admin_table.ts の表に行を足すか、tests/playwright/admin-settings-coverage-allowlist.tsv に理由を書く)']);
});

test('an allowlisted key passes, and a stale or covered allowlist row is a problem', () => {
  const ok = run({ declarations: [decl('a'), decl('b')], allowRows: [{ route: 'meta:c', line: 2 }] });
  assert.deepEqual(ok.problems, []);
  assert.equal(ok.allowed, 1);
  const bad = run({ allowRows: [{ route: 'meta:gone', line: 2 }, { route: 'meta:c', line: 3 }] });
  assert.equal(bad.problems.length, 2);
  assert.match(bad.problems[0], /画面に無いキー \(行を消す\): meta:gone/);
  assert.match(bad.problems[1], /meta:c は spec が宣言している/);
});

test('declarations of an unknown key, an untracked spec or a skipped test do not count', () => {
  const r = run({ declarations: [decl('a'), decl('b'), decl('c', { skipped: true }), decl('d'), decl('c', { file: 'tests/playwright/specs/new.spec.ts' })] });
  assert.equal(r.problems.length, 4);
  assert.match(r.problems.join('\n'), /skip \/ fixme/);
  assert.match(r.problems.join('\n'), /画面に無いキー \(消すか直す\): meta:d/);
  assert.match(r.problems.join('\n'), /git が追跡していない/);
  assert.match(r.problems.join('\n'), /meta:c: どの spec の行も確かめていない/);
});

test('too few keys, a missing marker key, or no declarations make the check untrusted', () => {
  assert.equal(run({ minKeys: 4 }).untrusted.length, 1);
  assert.match(run({ requiredKeys: ['zz'] }).untrusted[0], /目印のキー zz/);
  assert.match(run({ declarations: [] }).untrusted.join('\n'), /spec の宣言を 1 件も拾えなかった/);
});

test('splitAllowlist sorts rows by kind and rejects others', () => {
  const r = splitAllowlist('# comment\nmeta:a\treason\npolicy:b\treason\nother\treason\n');
  assert.deepEqual(r.meta.map((x) => x.route), ['meta:a']);
  assert.deepEqual(r.policy.map((x) => x.route), ['policy:b']);
  assert.equal(r.problems.length, 1);
});
