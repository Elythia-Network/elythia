/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// shard-plan.mjs のテスト (#4026)。`node --test tests/playwright/shard-plan.test.mjs`
// で流す (playwright.yml の plan job)。
//
// 割り振りの性質 (全ファイルがちょうど 1 回ずつ、偏りが小さい、決まった結果)
// と、実際のリポジトリの記録が古くなっていないことを見る。

import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import {
  durationsFromReports,
  listSpecFiles,
  loadDurations,
  mergeDurations,
  parseShard,
  planShards,
  staleEntries,
  writesMeta,
} from './shard-plan.mjs';

const here = fileURLToPath(new URL('.', import.meta.url));

test('every file lands in exactly one shard', () => {
  const files = Array.from({ length: 37 }, (_, i) => `specs/f${String(i).padStart(2, '0')}.spec.ts`);
  const durations = Object.fromEntries(files.map((f, i) => [f, (i * 7) % 13 + 1]));
  const shards = planShards(files, durations, 5);
  assert.equal(shards.length, 5);
  const flat = shards.flat();
  assert.deepEqual([...flat].sort(), files);
  assert.equal(new Set(flat).size, files.length);
});

test('heavy files are spread instead of following the file order', () => {
  // 並び順で切ると、前半が軽く後半が重くなる形 (今の CI と同じ偏り)
  const files = [
    ...Array.from({ length: 20 }, (_, i) => `specs/api/a${String(i).padStart(2, '0')}.spec.ts`),
    ...Array.from({ length: 20 }, (_, i) => `specs/ui/u${String(i).padStart(2, '0')}.spec.ts`),
  ];
  const durations = Object.fromEntries(files.map((f) => [f, f.includes('/ui/') ? 20 : 1]));
  const sums = planShards(files, durations, 4).map((s) => s.reduce((a, f) => a + durations[f], 0));
  assert.ok(Math.max(...sums) - Math.min(...sums) <= 1, `uneven shards: ${sums}`);
});

test('a file without a record is weighted by the mean', () => {
  // 平均は 34 秒。記録の無い new は重いものとして 1 つの shard を占める。
  // 0 や 1 として扱うと、軽い b と同じ shard に入る
  const durations = { 'specs/a.spec.ts': 100, 'specs/b.spec.ts': 1, 'specs/c.spec.ts': 1 };
  const files = [...Object.keys(durations), 'specs/new.spec.ts'];
  const shards = planShards(files, durations, 3);
  assert.ok(shards.some((s) => s.length === 1 && s[0] === 'specs/new.spec.ts'), JSON.stringify(shards));
});

test('the plan is deterministic', () => {
  const files = listSpecFiles(here);
  const durations = loadDurations(here);
  assert.deepEqual(planShards(files, durations, 6), planShards([...files].reverse(), durations, 6));
});

test('staleEntries lists records of removed specs', () => {
  const durations = { 'specs/a.spec.ts': 1, 'specs/gone.spec.ts': 2 };
  assert.deepEqual(staleEntries(['specs/a.spec.ts', 'specs/new.spec.ts'], durations), ['specs/gone.spec.ts']);
});

test('mergeDurations keeps other records and drops removed specs', () => {
  const files = ['specs/a.spec.ts', 'specs/b.spec.ts', 'specs/new.spec.ts'];
  const recorded = { 'specs/a.spec.ts': 1, 'specs/b.spec.ts': 2, 'specs/gone.spec.ts': 3 };
  assert.deepEqual(mergeDurations(files, recorded, { 'specs/b.spec.ts': 5, 'specs/new.spec.ts': 4 }), {
    'specs/a.spec.ts': 1,
    'specs/b.spec.ts': 5,
    'specs/new.spec.ts': 4,
  });
});

test('parseShard accepts i/N and rejects the rest', () => {
  assert.deepEqual(parseShard('2/6'), { current: 2, total: 6 });
  for (const bad of ['0/4', '5/4', '1/0', '2', 'a/b', '']) {
    assert.throws(() => parseShard(bad), undefined, bad);
  }
});

test('writesMeta detects the endpoints that write the meta', () => {
  assert.equal(writesMeta("await callApi(request, 'admin/update-meta', { i })"), true);
  assert.equal(writesMeta("r.url().includes('/api/admin/update-meta') && r.ok()"), true);
  assert.equal(writesMeta('await callApi(request, `admin/roles/update-default-policies`, {})'), true);
  assert.equal(writesMeta("await callApi(request, 'admin/meta', { i })"), false);
  assert.equal(writesMeta("await callApi(request, 'admin/update-meta-secrets', { i })"), false);
  // meta を書く helper (fixtures/admin_table.ts) を呼ぶ spec も meta を書く
  assert.equal(writesMeta('({ page }) => runMetaRow({ page }, row)'), true);
  assert.equal(writesMeta('import { runMetaRowLike } from x; runMetaRowLike(a)'), false);
});

test('durationsFromReports sums every attempt per file', () => {
  const report = {
    suites: [{
      specs: [
        { file: 'upstream/x.spec.ts', tests: [{ results: [{ duration: 1500 }, { duration: 500 }] }] },
        { file: 'upstream/x.spec.ts', tests: [{ results: [{ duration: 1000 }] }] },
      ],
      suites: [{ specs: [{ file: 'elythia/y.spec.ts', tests: [{ results: [{ duration: 250 }] }] }] }],
    }],
  };
  assert.deepEqual(durationsFromReports([report]), {
    'specs/upstream/x.spec.ts': 3,
    'specs/elythia/y.spec.ts': 0.3,
  });
});

// ここから実際のリポジトリの状態を見る

test('spec listing finds the known specs', () => {
  const files = listSpecFiles(here);
  // 抽出が空振りしても緑にならないよう、実在するものを名指しで要求する
  assert.ok(files.length >= 300, `only ${files.length} spec files found`);
  for (const f of ['specs/upstream/ui/route_matrix.spec.ts', 'specs/elythia/ui/route_not_found_guard.spec.ts']) {
    assert.ok(files.includes(f), `${f} not listed`);
  }
});

test('no test file hides from the listing under a name Playwright would run', () => {
  // config の testMatch は listSpecFiles の一覧 (`.spec.ts`) に置き換わるので、
  // Playwright の既定 (`*.{spec,test}.{js,ts,mjs,...}`) なら流れるはずのファイルが
  // 黙って流れなくなる。そういう名前のファイルを置かせない
  const stray = [];
  const walk = (dir) => {
    for (const ent of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, ent.name);
      if (ent.isDirectory()) walk(p);
      else if (/\.(spec|test)\.[cm]?[jt]sx?$/.test(ent.name) && !ent.name.endsWith('.spec.ts')) stray.push(p);
    }
  };
  walk(join(here, 'specs'));
  assert.deepEqual(stray, []);
});

test('meta writers are detected in the real specs', () => {
  const meta = listSpecFiles(here).filter((f) => writesMeta(readFileSync(join(here, f), 'utf-8')));
  assert.ok(meta.length >= 10, `only ${meta.length} meta writers found`);
  for (const f of ['specs/upstream/ui/admin_branding_save.spec.ts', 'specs/upstream/ui/public_pages.spec.ts']) {
    assert.ok(meta.includes(f), `${f} not detected`);
  }
});

test('the duration record has no entry for a removed spec', () => {
  const stale = staleEntries(listSpecFiles(here), loadDurations(here));
  assert.deepEqual(stale, [], 'remove these from shard-durations.json (node shard-plan.mjs update ...)');
});

test('the duration record covers most specs', () => {
  // 新しい spec は記録が無くても平均で割り振られるが、記録の無い spec が
  // 増えるほど偏る。2 割を超えたら記録を作り直す
  const files = listSpecFiles(here);
  const durations = loadDurations(here);
  const missing = files.filter((f) => typeof durations[f] !== 'number');
  assert.ok(missing.length <= files.length * 0.2, `${missing.length} specs have no record: run node shard-plan.mjs update`);
});
