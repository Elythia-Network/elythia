/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// @ts-check

/**
 * Fails when an admin setting or a role policy has no spec row that declares
 * it (#4066).
 *
 * Usage (from the repository root or anywhere):
 *   node tests/playwright/admin-settings-coverage.mjs
 *
 * Exit codes:
 *   0 = every key is declared by a spec or listed in the allowlist
 *   1 = some key is missing, or a declaration or an allowlist row is stale
 *   2 = the check could not be trusted (a parser, the spec listing, or a lower bound failed)
 *
 * - meta keys: the keys of the object the admin pages pass to
 *   `admin/update-meta` (the second argument of the call, read with the
 *   TypeScript syntax tree; object literals inside spreads and conditionals are
 *   followed, and a non-literal argument must be resolved by RESOLVED_ARGS).
 *   Rows declare them with `coversMeta('<key>')` (fixtures/admin_table.ts).
 * - policy keys: `data-policy-key` of every `<XFolder>` in the role policy
 *   editor (read with the Vue template compiler). Each must match the policy
 *   the folder edits (`v-model:policyMeta`). Rows declare them with
 *   `coversPolicies('<key>')`.
 * - declarations come from `playwright test --list --reporter=json`, as in
 *   route-coverage.mjs.
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

/** The annotation types of `coversMeta` / `coversPolicies` (fixtures/admin_table.ts). */
export const META_TYPE = 'meta';
export const POLICY_TYPE = 'policy';
export const ADMIN_PAGES = ['frontend/packages/frontend/src/pages/admin/', 'frontend/packages/frontend/src/pages/instance-info.vue'];
export const POLICY_EDITOR = 'frontend/packages/frontend/src/pages/admin/roles.policy-editor.vue';
export const ALLOWLIST = 'tests/playwright/admin-settings-coverage-allowlist.tsv';
/**
 * Lower bounds and marker keys, so that a renamed call or a changed page does
 * not turn the check into one that finds nothing and passes. The markers come
 * from pages that differ in how they call update-meta (a plain literal, a
 * spread inside a conditional, a resolved argument).
 */
export const MIN_META_KEYS = 100;
export const REQUIRED_META_KEYS = ['name', 'themeColor', 'chunkedUploadEnabled', 'disableRegistration'];
export const MIN_POLICY_KEYS = 45;
export const REQUIRED_POLICY_KEYS = ['gtlAvailable', 'canImportUserLists', 'optOutNotificationTypes'];
/**
 * A non-literal second argument of `admin/update-meta`: the identifier it
 * indexes, and the file that defines it. The keys of every object literal in
 * that definition count.
 */
export const RESOLVED_ARGS = { registrationModePatch: 'frontend/packages/frontend/src/utility/registration-mode.ts' };
/**
 * `v-model:policyMeta` bindings whose name does not follow `policyMetaModel.<key>`
 * or `<key>Meta`.
 */
export const POLICY_META_ALIASES = { optOutPolicyMeta: 'optOutNotificationTypes' };

/**
 * Collects the property names of the object literals in `node`, following
 * spreads, conditionals and parentheses. Returns null for anything else.
 *
 * @param {import('typescript').Node} node
 * @param {import('typescript').SourceFile} file
 * @returns {string[] | null}
 */
function literalKeys(node, file) {
  if (ts.isParenthesizedExpression(node)) return literalKeys(node.expression, file);
  if (ts.isConditionalExpression(node)) {
    const a = literalKeys(node.whenTrue, file);
    const b = literalKeys(node.whenFalse, file);
    return a && b ? [...a, ...b] : null;
  }
  if (!ts.isObjectLiteralExpression(node)) return null;
  /** @type {string[]} */
  const keys = [];
  for (const p of node.properties) {
    if (ts.isSpreadAssignment(p)) {
      const inner = literalKeys(p.expression, file);
      if (inner == null) return null;
      keys.push(...inner);
    } else if (p.name != null && (ts.isIdentifier(p.name) || ts.isStringLiteralLike(p.name))) {
      keys.push(p.name.text);
    } else {
      return null;
    }
  }
  return keys;
}

/**
 * Extracts the `admin/update-meta` keys from a source (a .vue file's script
 * blocks, or a .ts file). `resolve(name)` returns the keys of a non-literal
 * argument `name[...]`, or null when it cannot.
 *
 * @param {string} source
 * @param {boolean} vue
 * @param {(name: string) => string[] | null} resolve
 * @returns {{ keys: string[], unresolved: string[] }}
 */
export function extractMetaKeys(source, vue, resolve) {
  /** @type {string[]} */
  const keys = [];
  /** @type {string[]} */
  const unresolved = [];
  const blocks = vue ? [...source.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)].map((m) => m[1]) : [source];
  for (const block of blocks) {
    const file = ts.createSourceFile('page.ts', block, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
    /** @param {import('typescript').Node} node */
    const visit = (node) => {
      if (ts.isCallExpression(node) && node.arguments.length >= 2
        && ts.isStringLiteralLike(node.arguments[0]) && node.arguments[0].text === 'admin/update-meta') {
        const arg = node.arguments[1];
        const lit = literalKeys(arg, file);
        if (lit != null) keys.push(...lit);
        else {
          const base = ts.isElementAccessExpression(arg) && ts.isIdentifier(arg.expression) ? arg.expression.text : ts.isIdentifier(arg) ? arg.text : null;
          const resolved = base == null ? null : resolve(base);
          if (resolved == null) unresolved.push(arg.getText(file));
          else keys.push(...resolved);
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(file);
  }
  return { keys, unresolved };
}

/**
 * Collects the keys of every object literal inside the declaration of `name`
 * in `source` (a .ts file).
 *
 * @param {string} source
 * @param {string} name
 * @returns {string[] | null}
 */
export function definitionKeys(source, name) {
  const file = ts.createSourceFile('def.ts', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  /** @type {string[] | null} */
  let found = null;
  /** @param {import('typescript').Node} node */
  const visit = (node) => {
    if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.name.text === name && node.initializer) {
      // `{ ... } as const` / `satisfies` / 括弧を外した、いちばん外側のオブジェクトのキーは数えない
      let top = node.initializer;
      while (ts.isAsExpression(top) || ts.isSatisfiesExpression(top) || ts.isParenthesizedExpression(top)) top = top.expression;
      /** @type {string[]} */
      const keys = [];
      /** @param {import('typescript').Node} n */
      const inner = (n) => {
        if (ts.isObjectLiteralExpression(n) && n !== top) {
          for (const p of n.properties) if (p.name != null && (ts.isIdentifier(p.name) || ts.isStringLiteralLike(p.name))) keys.push(p.name.text);
        }
        ts.forEachChild(n, inner);
      };
      inner(node.initializer);
      found = keys;
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  return found;
}

/**
 * Extracts the policy keys of the role policy editor: `data-policy-key` of
 * every `<XFolder>`. A folder without one, or one that does not match the
 * policy it edits, is a problem.
 *
 * @param {string} source the .vue file
 * @param {(source: string) => any} parseTemplate returns the template AST root (vue/compiler-sfc)
 * @returns {{ keys: string[], problems: string[] }}
 */
export function extractPolicyKeys(source, parseTemplate) {
  /** @type {string[]} */
  const keys = [];
  /** @type {string[]} */
  const problems = [];
  /** @param {any} node */
  const visit = (node) => {
    if (node.type === 1 && node.tag === 'XFolder') {
      const attr = node.props.find((/** @type {any} */ p) => p.type === 6 && p.name === 'data-policy-key');
      const model = node.props.find((/** @type {any} */ p) => p.type === 7 && p.name === 'model' && p.arg?.content === 'policyMeta');
      const line = node.loc.start.line;
      if (attr?.value?.content == null) problems.push(`${POLICY_EDITOR}:${line}: <XFolder> に data-policy-key が無い`);
      else if (model?.exp?.content == null) problems.push(`${POLICY_EDITOR}:${line}: <XFolder> に v-model:policyMeta が無い`);
      else {
        const key = attr.value.content;
        const bound = String(model.exp.content);
        const expected = bound.startsWith('policyMetaModel.') ? bound.slice('policyMetaModel.'.length)
          : Object.hasOwn(POLICY_META_ALIASES, bound) ? POLICY_META_ALIASES[/** @type {keyof typeof POLICY_META_ALIASES} */ (bound)]
          : bound.endsWith('Meta') ? bound.slice(0, -'Meta'.length) : null;
        if (expected !== key) problems.push(`${POLICY_EDITOR}:${line}: data-policy-key="${key}" が、編集するポリシー (${bound}) と合わない`);
        keys.push(key);
      }
    }
    for (const child of node.children ?? []) visit(child);
    // v-if の枝は branches の下にある
    for (const b of node.branches ?? []) visit(b);
  };
  visit(parseTemplate(source));
  return { keys, problems };
}

/**
 * Compares the keys of one kind with the declarations and the allowlist rows
 * of that kind (`<kind>:<key>` in the first column).
 *
 * @param {{ kind: string, keys: string[], declarations: import('./route-coverage.mjs').Declaration[], tracked: Set<string>, allowRows: { route: string, line: number }[], minKeys: number, requiredKeys: string[], where: string }} input
 * @returns {{ problems: string[], untrusted: string[], covered: number, allowed: number }}
 */
export function checkKind({ kind, keys, declarations, tracked, allowRows, minKeys, requiredKeys, where }) {
  /** @type {string[]} */
  const problems = [];
  /** @type {string[]} */
  const untrusted = [];
  const keySet = new Set(keys);
  if (keySet.size < minKeys) untrusted.push(`${kind}: キーが ${keySet.size} 個しか拾えなかった (下限 ${minKeys})`);
  for (const k of requiredKeys) {
    if (!keySet.has(k)) untrusted.push(`${kind}: 目印のキー ${k} が拾えなかった`);
  }
  if (declarations.length === 0) untrusted.push(`${kind}: spec の宣言を 1 件も拾えなかった`);

  /** @type {Set<string>} */
  const declared = new Set();
  for (const d of declarations) {
    if (!tracked.has(d.file)) {
      problems.push(`${d.file}:${d.line}: git が追跡していない spec の宣言は数えない (git add する): ${kind}:${d.route}`);
      continue;
    }
    if (d.skipped) {
      problems.push(`${d.file}:${d.line}: skip / fixme のテストの宣言は数えない (流れるテストに付ける): ${kind}:${d.route}`);
      continue;
    }
    if (!keySet.has(d.route)) {
      problems.push(`${d.file}:${d.line}: ${where}に無いキー (消すか直す): ${kind}:${d.route}`);
      continue;
    }
    declared.add(d.route);
  }

  /** @type {Set<string>} */
  const allowed = new Set();
  for (const row of allowRows) {
    const key = row.route.slice(kind.length + 1);
    if (!keySet.has(key)) problems.push(`${ALLOWLIST}:${row.line}: ${where}に無いキー (行を消す): ${row.route}`);
    else if (declared.has(key)) problems.push(`${ALLOWLIST}:${row.line}: ${row.route} は spec が宣言している。この行を消す`);
    else allowed.add(key);
  }

  for (const k of [...keySet].sort()) {
    if (!declared.has(k) && !allowed.has(k)) {
      problems.push(`${kind}:${k}: どの spec の行も確かめていない (fixtures/admin_table.ts の表に行を足すか、${ALLOWLIST} に理由を書く)`);
    }
  }
  return { problems, untrusted, covered: declared.size, allowed: allowed.size };
}

/**
 * Splits the allowlist into the rows of each kind.
 *
 * @param {string} text
 * @returns {{ meta: { route: string, line: number }[], policy: { route: string, line: number }[], problems: string[] }}
 */
export function splitAllowlist(text) {
  const list = parseList(text, ALLOWLIST);
  /** @type {{ route: string, line: number }[]} */
  const meta = [];
  /** @type {{ route: string, line: number }[]} */
  const policy = [];
  const problems = [...list.problems];
  for (const row of list.rows) {
    if (row.route.startsWith(`${META_TYPE}:`)) meta.push(row);
    else if (row.route.startsWith(`${POLICY_TYPE}:`)) policy.push(row);
    else problems.push(`${ALLOWLIST}:${row.line}: 1 列目は meta:<キー> か policy:<キー>: ${row.route}`);
  }
  return { meta, policy, problems };
}

/** @returns {string[]} the admin page files tracked by git */
function adminPages() {
  const out = execFileSync('git', ['ls-files', '-z', '--', `${ADMIN_PAGES[0]}*.vue`, ADMIN_PAGES[1]], { cwd: repoRoot, encoding: 'utf-8' });
  return out.split('\0').filter((f) => f !== '');
}

/**
 * @param {(line: string) => void} out
 * @returns {number} the exit code
 */
export function run(out) {
  /** @type {string[]} */
  const untrusted = [];
  /** @type {string[]} */
  const problems = [];
  /** @type {string[]} */
  const summary = [];
  try {
    const shadows = shadowingFiles(ignoredFiles());
    if (shadows.length > 0) {
      out(`admin-settings-coverage: 検査を信用できない: .ts を隠す .js がある (消してから流す): ${shadows.join(', ')}`);
      return 2;
    }
    /** @param {string} name */
    const resolve = (name) => {
      const file = /** @type {Record<string, string>} */ (RESOLVED_ARGS)[name];
      return file == null ? null : definitionKeys(readFileSync(join(repoRoot, file), 'utf-8'), name);
    };
    /** @type {string[]} */
    const metaKeys = [];
    for (const f of adminPages()) {
      const r = extractMetaKeys(readFileSync(join(repoRoot, f), 'utf-8'), f.endsWith('.vue'), resolve);
      metaKeys.push(...r.keys);
      for (const u of r.unresolved) untrusted.push(`${f}: admin/update-meta の引数のキーを読めない (RESOLVED_ARGS に足す): ${u}`);
    }
    const frontendRequire = createRequire(join(repoRoot, 'frontend/packages/frontend/package.json'));
    const sfc = frontendRequire('vue/compiler-sfc');
    const policy = extractPolicyKeys(readFileSync(join(repoRoot, POLICY_EDITOR), 'utf-8'), (src) => {
      const { descriptor, errors } = sfc.parse(src);
      if (errors.length > 0) throw new Error(`${POLICY_EDITOR} を解析できない: ${errors[0]}`);
      return descriptor.template.ast;
    });
    problems.push(...policy.problems);

    const report = listTests();
    const tracked = trackedSpecs();
    const allow = splitAllowlist(readFileSync(join(repoRoot, ALLOWLIST), 'utf-8'));
    untrusted.push(...allow.problems);
    for (const [kind, keys, rows, minKeys, requiredKeys, where] of /** @type {const} */ ([
      [META_TYPE, metaKeys, allow.meta, MIN_META_KEYS, REQUIRED_META_KEYS, '管理画面の admin/update-meta'],
      [POLICY_TYPE, policy.keys, allow.policy, MIN_POLICY_KEYS, REQUIRED_POLICY_KEYS, 'ロールのポリシーの編集画面'],
    ])) {
      const result = checkKind({ kind, keys, declarations: collectDeclarations(report, kind), tracked, allowRows: rows, minKeys, requiredKeys: [...requiredKeys], where });
      problems.push(...result.problems);
      untrusted.push(...result.untrusted);
      summary.push(`${kind} の宣言 ${result.covered}、許可リスト ${result.allowed}`);
    }
  } catch (err) {
    out(`admin-settings-coverage: 検査できなかった: ${err instanceof Error ? err.message : String(err)}`);
    return 2;
  }
  if (untrusted.length > 0) {
    for (const p of untrusted) out(`admin-settings-coverage: 検査を信用できない: ${p}`);
    return 2;
  }
  if (problems.length > 0) {
    for (const p of problems) out(`admin-settings-coverage: ${p}`);
    return 1;
  }
  out(`admin-settings-coverage: ok (${summary.join('、')})`);
  return 0;
}

if (process.argv[1] != null && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href) {
  process.exit(run((line) => console.log(line)));
}
