/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// spec ファイルを所要時間で shard に割り振る (#4026)。
//
// Playwright の `--shard=i/N` はファイルの並び順で切るだけなので、軽い
// `upstream/api` が前半の shard に、重い `upstream/ui` が後半の shard に集まる
// (直近の CI では 1/4 が約 100 秒、3/4 と 4/4 が約 430 秒)。Playwright には
// 時間で割り振る機能が無いので、ファイルごとの所要時間 (`shard-durations.json`)
// から、合計が揃うように割り振る。
//
// playwright.config.ts が `PLAYWRIGHT_SHARD=i/N` を受け取ったときに使う。
// 所要時間の記録の更新は `node shard-plan.mjs update <results.json>...`
// (docs/playwright.md)。
//
// 依存を足さないために、runner の image にも host にもある node の標準
// ライブラリだけで書く。`import.meta` は使わない。Playwright は config から
// 読み込んだこのファイルも CommonJS に変換するので、`import.meta` があると
// config の読み込みで落ちる。置き場所は呼び出し側から `root` で渡す。

import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { basename, dirname, join, relative, resolve } from 'node:path';

/** Path of the recorded per-file durations, relative to tests/playwright. */
export const DURATIONS_FILE = 'shard-durations.json';

/**
 * Lists every spec file under `specs/`, as paths relative to tests/playwright
 * with forward slashes, sorted.
 *
 * @param {string} root tests/playwright directory
 * @returns {string[]}
 */
export function listSpecFiles(root) {
  const out = [];
  const walk = (dir) => {
    for (const ent of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, ent.name);
      if (ent.isDirectory()) walk(p);
      else if (ent.name.endsWith('.spec.ts')) out.push(relative(root, p).split('\\').join('/'));
    }
  };
  walk(join(root, 'specs'));
  return out.sort();
}

/**
 * Specs that run only on the backup stack (backup-stack/compose.yml, #4102).
 * They are not split into shards and have no duration record.
 */
export const BACKUP_SPEC_DIR = 'specs/backup/';

/**
 * Lists the spec files the shards share: every spec but the ones of the backup
 * stack.
 *
 * @param {string} root tests/playwright directory
 * @returns {string[]}
 */
export function listShardedSpecFiles(root) {
  return listSpecFiles(root).filter((f) => !f.startsWith(BACKUP_SPEC_DIR));
}

/**
 * Endpoints that write the instance meta. `admin/roles/update-default-policies`
 * stores the default role policies in the meta as well.
 */
export const META_WRITERS = ['admin/update-meta', 'admin/roles/update-default-policies'];

/**
 * Helpers that write the instance meta for the spec (fixtures/admin_table.ts).
 * A spec that calls one writes the meta even when its own source has no
 * endpoint name.
 */
export const META_WRITING_HELPERS = ['runMetaRow'];

/**
 * Reports whether a spec file writes the instance meta. Such specs run in the
 * `meta` project after every other spec (playwright.config.ts).
 *
 * @param {string} content source of the spec file
 * @returns {boolean}
 */
export function writesMeta(content) {
  // 直前の引用符は要求しない。`callApi(request, 'admin/update-meta')` だけでなく、
  // `r.url().includes('/api/admin/update-meta')` のように URL の一部として書いた
  // 形も拾う。後ろは引用符で切り、`admin/update-meta-foo` のような別の名前は拾わない
  // helper を呼ぶ spec は、本文に endpoint の名前が無くても meta を書く (#4066)。
  // endpoint の名前で拾うだけだと、説明のコメントの書き方しだいで main に入ってしまう
  if (META_WRITING_HELPERS.some((h) => new RegExp(`\\b${h}\\(`).test(content))) return true;
  return META_WRITERS.some((ep) => new RegExp(`${ep.replace(/\//g, '\\/')}['"\`]`).test(content));
}

/**
 * Parses `i/N` (one-based) as used by `--shard`.
 *
 * @param {string} value
 * @returns {{ current: number, total: number }}
 */
export function parseShard(value) {
  const m = /^(\d+)\/(\d+)$/.exec(value);
  if (m === null) throw new Error(`PLAYWRIGHT_SHARD must look like 2/6: ${JSON.stringify(value)}`);
  const current = Number(m[1]);
  const total = Number(m[2]);
  if (total < 1 || current < 1 || current > total) {
    throw new Error(`PLAYWRIGHT_SHARD out of range: ${value}`);
  }
  return { current, total };
}

/**
 * Splits files into `total` shards so that the summed durations are as even
 * as possible (longest first, each into the currently lightest shard). A file
 * without a recorded duration is assumed to take the mean of the recorded ones.
 * The result is deterministic for the same input.
 *
 * @param {string[]} files
 * @param {Record<string, number>} durations seconds per file
 * @param {number} total
 * @returns {string[][]} files per shard, each sorted
 */
export function planShards(files, durations, total) {
  const known = files.map((f) => durations[f]).filter((d) => typeof d === 'number');
  const mean = known.length > 0 ? known.reduce((a, b) => a + b, 0) / known.length : 1;
  const weighted = files
    .map((f) => ({ file: f, sec: typeof durations[f] === 'number' ? durations[f] : mean }))
    .sort((a, b) => b.sec - a.sec || (a.file < b.file ? -1 : a.file > b.file ? 1 : 0));
  const shards = Array.from({ length: total }, () => ({ sec: 0, files: [] }));
  for (const w of weighted) {
    let min = 0;
    for (let i = 1; i < total; i++) if (shards[i].sec < shards[min].sec) min = i;
    shards[min].sec += w.sec;
    shards[min].files.push(w.file);
  }
  return shards.map((s) => s.files.sort());
}

/**
 * Returns recorded entries whose spec file no longer exists. Such entries skew
 * the mean assumed for new files, so the check fails on them.
 *
 * @param {string[]} files
 * @param {Record<string, number>} durations
 * @returns {string[]}
 */
export function staleEntries(files, durations) {
  const set = new Set(files);
  return Object.keys(durations).filter((f) => !set.has(f)).sort();
}

/**
 * Loads `shard-durations.json`.
 *
 * @param {string} root tests/playwright directory
 * @returns {Record<string, number>}
 */
export function loadDurations(root) {
  return JSON.parse(readFileSync(join(root, DURATIONS_FILE), 'utf-8'));
}

/**
 * Sums the duration of every attempt per spec file in Playwright JSON reports.
 * Retries are counted because they take time in CI as well.
 *
 * @param {unknown[]} reports parsed results.json
 * @returns {Record<string, number>} seconds per file, relative to tests/playwright
 */
export function durationsFromReports(reports) {
  const out = {};
  const visit = (suite) => {
    for (const spec of suite.specs ?? []) {
      const file = `specs/${spec.file}`;
      for (const t of spec.tests ?? []) {
        for (const r of t.results ?? []) out[file] = (out[file] ?? 0) + (r.duration ?? 0) / 1000;
      }
    }
    for (const s of suite.suites ?? []) visit(s);
  };
  for (const report of reports) for (const s of report.suites ?? []) visit(s);
  for (const f of Object.keys(out)) out[f] = Math.round(out[f] * 10) / 10;
  return out;
}

/**
 * Overlays fresh durations on the recorded ones and drops records of specs
 * that no longer exist. Files missing from the fresh reports keep their record,
 * so a report from a single shard updates only that shard's files.
 *
 * @param {string[]} files current spec files
 * @param {Record<string, number>} recorded
 * @param {Record<string, number>} fresh
 * @returns {Record<string, number>} sorted by file
 */
export function mergeDurations(files, recorded, fresh) {
  const out = {};
  for (const f of files) {
    if (typeof fresh[f] === 'number') out[f] = fresh[f];
    else if (typeof recorded[f] === 'number') out[f] = recorded[f];
  }
  return out;
}

// `node shard-plan.mjs update [<results.json>...]` で記録を更新する。渡した report に
// あるファイルだけを書き換え、他の記録は残す (shard ごとの結果を 1 つずつ渡せる)。
// 消えた spec の記録は落とす。report を渡さなければ、消えた spec を落とすだけ。
// `node shard-plan.mjs show <N>` で割り振りと各 shard の見込み時間を出す。
if (process.argv[1] !== undefined && basename(process.argv[1]) === 'shard-plan.mjs') {
  const here = dirname(resolve(process.argv[1]));
  const [cmd, ...args] = process.argv.slice(2);
  if (cmd === 'update') {
    // バックアップのスタックの spec は shard に入らないので、記録しない。CI の artifact を
    // まとめて渡したときに、その結果が混ざっても落とす
    const files = listShardedSpecFiles(here);
    const merged = mergeDurations(files, loadDurations(here), durationsFromReports(args.map((p) => JSON.parse(readFileSync(p, 'utf-8')))));
    writeFileSync(join(here, DURATIONS_FILE), `${JSON.stringify(merged, null, 2)}\n`);
    console.log(`recorded ${Object.keys(merged).length} of ${files.length} spec files`);
  } else if (cmd === 'show') {
    const total = Number(args[0]);
    const durations = loadDurations(here);
    const files = listShardedSpecFiles(here);
    planShards(files, durations, total).forEach((s, i) => {
      const sec = s.reduce((a, f) => a + (durations[f] ?? 0), 0);
      console.log(`${i + 1}/${total}: ${s.length} files, ~${Math.round(sec)}s recorded`);
    });
  } else {
    throw new Error('usage: node shard-plan.mjs update [<results.json>...] | show <N>');
  }
}
