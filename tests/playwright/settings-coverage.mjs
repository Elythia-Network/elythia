/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// @ts-check

/**
 * Fails when a device preference that a settings page binds with
 * `prefer.model` has no spec row that declares it (#4056).
 *
 * Usage (from the repository root or anywhere):
 *   node tests/playwright/settings-coverage.mjs
 *
 * Exit codes:
 *   0 = every key is declared by a spec or listed in the allowlist
 *   1 = some key is missing, or a declaration or an allowlist row is stale
 *   2 = the check could not be trusted (a parser, the spec listing, or the lower bound failed)
 *
 * Each row of the settings table declares its key with `coversPrefs('<key>')`
 * (fixtures/settings_table.ts). The keys come from the `prefer.model('<key>')`
 * calls in the `<script setup>` blocks of the settings pages, walked with the
 * TypeScript syntax tree. Keys a page changes in other ways (`prefer.commit`,
 * for example the theme or the menu) are not part of this check. The
 * declarations come from `playwright test --list --reporter=json`, as in
 * route-coverage.mjs.
 */

import { execFileSync } from 'node:child_process';
import { readFileSync, realpathSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { collectDeclarations, ignoredFiles, listTests, parseList, shadowingFiles, trackedSpecs } from './route-coverage.mjs';

const here = fileURLToPath(new URL('.', import.meta.url));
const repoRoot = join(here, '..', '..');
const require = createRequire(join(here, 'package.json'));
/** @type {typeof import('typescript')} */
const ts = require('typescript');

/** The annotation type of `coversPrefs` (fixtures/settings_table.ts). */
export const ANNOTATION_TYPE = 'pref';
export const SETTINGS_PAGES = 'frontend/packages/frontend/src/pages/settings/';
export const ALLOWLIST = 'tests/playwright/settings-coverage-allowlist.tsv';
/**
 * Lower bounds, so that a renamed `prefer.model` or a changed page layout does
 * not turn the check into one that finds nothing and passes. The marker keys
 * come from four pages with many keys, one each.
 */
export const MIN_KEYS = 80;
export const REQUIRED_KEYS = ['squareAvatars', 'deck.columnAlign', 'sound.masterVolume', 'emojiPaletteForMain'];

/**
 * Extracts the keys of `prefer.model('<key>', ...)` calls from the
 * `<script setup>` blocks of a Vue single-file component.
 *
 * @param {string} source the .vue file
 * @returns {string[]}
 */
export function extractKeys(source) {
  /** @type {string[]} */
  const keys = [];
  for (const m of source.matchAll(/<script\b[^>]*\bsetup\b[^>]*>([\s\S]*?)<\/script>/g)) {
    const file = ts.createSourceFile('page.ts', m[1], ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
    /** @param {import('typescript').Node} node */
    const visit = (node) => {
      if (ts.isCallExpression(node)
        && ts.isPropertyAccessExpression(node.expression)
        && node.expression.name.text === 'model'
        && ts.isIdentifier(node.expression.expression)
        && node.expression.expression.text === 'prefer'
        && node.arguments.length > 0
        && ts.isStringLiteralLike(node.arguments[0])) {
        keys.push(node.arguments[0].text);
      }
      ts.forEachChild(node, visit);
    };
    visit(file);
  }
  return keys;
}

/**
 * Compares the keys with the declarations and the allowlist.
 *
 * @param {{ keys: string[], declarations: import('./route-coverage.mjs').Declaration[], tracked: Set<string>, allowlist: string }} input
 * @returns {{ problems: string[], untrusted: string[], covered: number, allowed: number }}
 */
export function check({ keys, declarations, tracked, allowlist }) {
  /** @type {string[]} */
  const problems = [];
  /** @type {string[]} */
  const untrusted = [];
  const keySet = new Set(keys);
  if (keySet.size < MIN_KEYS) untrusted.push(`キーが ${keySet.size} 個しか拾えなかった (下限 ${MIN_KEYS})`);
  for (const k of REQUIRED_KEYS) {
    if (!keySet.has(k)) untrusted.push(`目印のキー ${k} が拾えなかった`);
  }
  if (declarations.length === 0) untrusted.push('spec の宣言を 1 件も拾えなかった');

  /** @type {Set<string>} */
  const declared = new Set();
  for (const d of declarations) {
    if (!tracked.has(d.file)) {
      problems.push(`${d.file}:${d.line}: git が追跡していない spec の宣言は数えない (git add する): ${d.route}`);
      continue;
    }
    if (d.skipped) {
      problems.push(`${d.file}:${d.line}: skip / fixme のテストの宣言は数えない (流れるテストに付ける): ${d.route}`);
      continue;
    }
    if (!keySet.has(d.route)) {
      problems.push(`${d.file}:${d.line}: 設定のページに無いキー (消すか直す): ${d.route}`);
      continue;
    }
    declared.add(d.route);
  }

  const list = parseList(allowlist, ALLOWLIST);
  untrusted.push(...list.problems);
  /** @type {Set<string>} */
  const allowed = new Set();
  for (const row of list.rows) {
    if (!keySet.has(row.route)) problems.push(`${ALLOWLIST}:${row.line}: 設定のページに無いキー (行を消す): ${row.route}`);
    else if (declared.has(row.route)) problems.push(`${ALLOWLIST}:${row.line}: ${row.route} は spec が宣言している。この行を消す`);
    else allowed.add(row.route);
  }

  for (const k of [...keySet].sort()) {
    if (!declared.has(k) && !allowed.has(k)) {
      problems.push(`${k}: どの spec の行も確かめていない (fixtures/settings_table.ts の表に行を足すか、${ALLOWLIST} に理由を書く)`);
    }
  }
  return { problems, untrusted, covered: declared.size, allowed: allowed.size };
}

/**
 * @param {ReturnType<typeof check>} result
 * @param {(line: string) => void} out
 * @returns {number} the exit code
 */
export function report(result, out) {
  if (result.untrusted.length > 0) {
    for (const p of result.untrusted) out(`settings-coverage: 検査を信用できない: ${p}`);
    return 2;
  }
  if (result.problems.length > 0) {
    for (const p of result.problems) out(`settings-coverage: ${p}`);
    return 1;
  }
  out(`settings-coverage: ok (spec の宣言 ${result.covered}、許可リスト ${result.allowed})`);
  return 0;
}

/** @returns {string[]} the settings page files tracked by git */
function settingsPages() {
  const out = execFileSync('git', ['ls-files', '-z', '--', `${SETTINGS_PAGES}*.vue`], { cwd: repoRoot, encoding: 'utf-8' });
  return out.split('\0').filter((f) => f !== '');
}

/**
 * @param {(line: string) => void} out
 * @returns {number} the exit code
 */
export function run(out) {
  /** @type {ReturnType<typeof check>} */
  let result;
  try {
    // 手元に残った .js は .ts の spec や fixture より先に読まれ、手元と CI で一覧が食い違う
    const shadows = shadowingFiles(ignoredFiles());
    if (shadows.length > 0) {
      out(`settings-coverage: 検査を信用できない: .ts を隠す .js がある (消してから流す): ${shadows.join(', ')}`);
      return 2;
    }
    const keys = settingsPages().flatMap((f) => extractKeys(readFileSync(join(repoRoot, f), 'utf-8')));
    result = check({
      keys,
      declarations: collectDeclarations(listTests(), ANNOTATION_TYPE),
      tracked: trackedSpecs(),
      allowlist: readFileSync(join(repoRoot, ALLOWLIST), 'utf-8'),
    });
  } catch (err) {
    out(`settings-coverage: 検査できなかった: ${err instanceof Error ? err.message : String(err)}`);
    return 2;
  }
  return report(result, out);
}

if (process.argv[1] != null && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href) {
  process.exit(run((line) => console.log(line)));
}
