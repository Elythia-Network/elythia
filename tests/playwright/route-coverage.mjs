/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// @ts-check
/**
 * Fails when a frontend route has no spec that declares it covers the route (#4035).
 *
 * Usage (from the repository root or anywhere):
 *   node tests/playwright/route-coverage.mjs
 *
 * Exit codes:
 *   0 = every route is declared by a spec or listed in one of the two lists
 *   1 = some route is missing, or a declaration or a list row is stale
 *   2 = the check could not be trusted (a parser, the spec listing, or the lower bound failed)
 *
 * spec は `coversRoutes('/notes/:noteId/:initialTab?')` (fixtures/ui_route.ts) で、確かめる
 * ルートを annotation として宣言する。ルートの一覧は router.definition.ts の `ROUTE_DEF` を
 * TypeScript の構文木で辿って作り、宣言の一覧は `playwright test --list --reporter=json` に
 * 解かせる。どちらも自前の正規表現では拾わない (CLAUDE.md Section 4「ゲートを書くとき」)。
 *
 * プラグインのルート (router.ts の routeDefsWithPlugins) は対象にしない。どのプラグインを
 * 入れるかは運営者ごとに違い、`ROUTE_DEF` には現れないため。
 */

import { execFileSync } from 'node:child_process';
import { readFileSync, realpathSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = fileURLToPath(new URL('.', import.meta.url));
const repoRoot = join(here, '..', '..');
const require = createRequire(join(here, 'package.json'));
/** @type {typeof import('typescript')} */
const ts = require('typescript');

/** The annotation type that declares a covered route. */
export const ANNOTATION_TYPE = 'route';

export const ROUTER_DEFINITION = 'frontend/packages/frontend/src/router.definition.ts';
export const ALLOWLIST = 'tests/playwright/route-coverage-allowlist.tsv';
export const PENDING = 'tests/playwright/route-coverage-pending.tsv';

/**
 * The spec files of the route table (fixtures/route_table.ts, #4041). Every route
 * not in the two lists must be declared by one of them as well, so that a route
 * is not left out of the table because another spec happens to declare it.
 */
export const TABLE_SPEC_PREFIX = 'tests/playwright/specs/elythia/ui/route_table_open_';

/**
 * Lower bounds, so that a renamed `ROUTE_DEF` or a changed listing format does not
 * turn the check into one that finds nothing and passes.
 *
 * 目印は、トップレベル・パラメータ付き・`/settings` の子・`/admin` の子・catch-all を 1 つずつ
 * 選ぶ。子を親と連結し損ねると、子の目印が見つからずに落ちる。
 */
export const MIN_ROUTES = 150;
export const REQUIRED_ROUTES = ['/notes/:noteId/:initialTab?', '/settings/profile', '/admin/roles/:id', '/:(*)'];

/**
 * Lists the full paths of the routes in router.definition.ts. Children are joined
 * to their parent; a child whose path is `/` is the parent itself and is not listed
 * again.
 *
 * @param {string} source the text of router.definition.ts
 * @returns {string[]} the full paths, in the order they are defined
 */
export function extractRoutes(source) {
  const sf = ts.createSourceFile(ROUTER_DEFINITION, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  /** @type {import('typescript').ArrayLiteralExpression | undefined} */
  let defs;
  for (const stmt of sf.statements) {
    if (!ts.isVariableStatement(stmt)) continue;
    for (const decl of stmt.declarationList.declarations) {
      if (ts.isIdentifier(decl.name) && decl.name.text === 'ROUTE_DEF' && decl.initializer != null) {
        const init = unwrap(decl.initializer);
        if (!ts.isArrayLiteralExpression(init)) throw new Error('ROUTE_DEF is not an array literal');
        defs = init;
      }
    }
  }
  if (defs == null) throw new Error('ROUTE_DEF was not found');
  /** @type {string[]} */
  const out = [];
  walkRoutes(defs, '', out);
  return out;
}

/** @param {import('typescript').Expression} node */
function unwrap(node) {
  while (ts.isAsExpression(node) || ts.isSatisfiesExpression(node) || ts.isParenthesizedExpression(node)) {
    node = node.expression;
  }
  return node;
}

/**
 * @param {import('typescript').ArrayLiteralExpression} array
 * @param {string} prefix
 * @param {string[]} out
 */
function walkRoutes(array, prefix, out) {
  for (const el of array.elements) {
    const obj = unwrap(/** @type {import('typescript').Expression} */ (el));
    if (!ts.isObjectLiteralExpression(obj)) throw new Error(`a route is not an object literal: ${el.getText()}`);
    /** @type {string | undefined} */
    let path;
    /** @type {import('typescript').ArrayLiteralExpression | undefined} */
    let children;
    for (const prop of obj.properties) {
      // `{ children }` や `...rest` は構文木から中身が分からない。黙って子を落とさずに落とす
      if (ts.isShorthandPropertyAssignment(prop) || ts.isSpreadAssignment(prop)) {
        throw new Error(`a route property cannot be read: ${prop.getText()}`);
      }
      if (!ts.isPropertyAssignment(prop)) continue;
      const key = prop.name.getText();
      const value = unwrap(prop.initializer);
      if (key === 'path') {
        // パスを式で組み立てると、構文木からは値が分からない。黙って飛ばさず落とす
        if (!ts.isStringLiteral(value) && !ts.isNoSubstitutionTemplateLiteral(value)) {
          throw new Error(`a route path is not a string literal: ${prop.getText()}`);
        }
        path = value.text;
      } else if (key === 'children') {
        if (!ts.isArrayLiteralExpression(value)) throw new Error(`children is not an array literal: ${prop.getText()}`);
        children = value;
      }
    }
    if (path == null) throw new Error(`a route has no path: ${obj.getText()}`);
    // 子の `/` は親そのもの (nirax は親のパスで子の `/` を選ぶ)
    if (prefix === '' || path !== '/') out.push(prefix + path);
    if (children != null) walkRoutes(children, prefix + path, out);
  }
}

/**
 * @typedef {{ file: string, line: number, title: string, route: string, skipped: boolean }} Declaration
 */

/**
 * Collects the declarations from the JSON report of `playwright test --list`.
 * The settings check (settings-coverage.mjs, #4056) passes its own annotation
 * type; `route` then holds the declared preference key.
 *
 * @param {any} report the parsed JSON report
 * @param {string} [type] the annotation type to collect
 * @returns {Declaration[]}
 */
export function collectDeclarations(report, type = ANNOTATION_TYPE) {
  if (report == null || !Array.isArray(report.suites)) throw new Error('the listing has no suites');
  /** @type {Map<string, Declaration>} */
  const seen = new Map();
  /** @param {any} suite */
  const walk = (suite) => {
    for (const spec of suite.specs ?? []) {
      for (const t of spec.tests ?? []) {
        if (!Array.isArray(t.annotations)) throw new Error(`a test has no annotations field: ${spec.file}:${spec.line}`);
        for (const a of t.annotations) {
          if (a.type !== type) continue;
          const d = {
            file: `tests/playwright/specs/${spec.file}`,
            line: spec.line,
            title: spec.title,
            route: String(a.description ?? ''),
            // test.skip / test.fixme / describe.skip は、一覧の時点で skipped になる
            skipped: t.expectedStatus === 'skipped',
          };
          // project が 2 つあっても、各 spec はどちらか 1 つにしか入らない。念のため重ねない
          seen.set(`${d.file}:${d.line}:${d.route}`, d);
        }
      }
    }
    for (const child of suite.suites ?? []) walk(child);
  };
  for (const suite of report.suites) walk(suite);
  return [...seen.values()];
}

/**
 * Parses a two-column TSV list. Blank lines and lines starting with `#` are skipped.
 *
 * @param {string} text
 * @param {string} name the file name, for messages
 * @returns {{ rows: { route: string, note: string, line: number }[], problems: string[] }}
 */
export function parseList(text, name) {
  /** @type {{ route: string, note: string, line: number }[]} */
  const rows = [];
  /** @type {string[]} */
  const problems = [];
  text.split('\n').forEach((raw, i) => {
    const line = raw.replace(/\r$/, '');
    if (line.trim() === '' || line.startsWith('#')) return;
    const cols = line.split('\t');
    if (cols.length !== 2 || cols[0] === '' || cols[1].trim() === '') {
      problems.push(`${name}:${i + 1}: \`<ルート>\\t<理由>\` の 2 列になっていない: ${line}`);
      return;
    }
    rows.push({ route: cols[0], note: cols[1], line: i + 1 });
  });
  return { rows, problems };
}

/**
 * Compares the routes with the declarations and the two lists.
 *
 * @param {{ routes: string[], declarations: Declaration[], tracked: Set<string>, allowlist: string, pending: string, tablePrefix?: string }} input
 * @returns {{ problems: string[], untrusted: string[], covered: number, allowed: number, pending: number }}
 */
export function check({ routes, declarations, tracked, allowlist, pending, tablePrefix }) {
  /** @type {string[]} */
  const problems = [];
  /** @type {string[]} */
  const untrusted = [];

  if (routes.length < MIN_ROUTES) untrusted.push(`ルートが ${routes.length} 個しか拾えなかった (下限 ${MIN_ROUTES})`);
  for (const r of REQUIRED_ROUTES) {
    if (!routes.includes(r)) untrusted.push(`目印のルート ${r} を拾えなかった`);
  }
  const routeSet = new Set(routes);
  if (routeSet.size !== routes.length) {
    const dup = routes.filter((r, i) => routes.indexOf(r) !== i);
    problems.push(`router.definition.ts に同じパスが 2 回ある: ${[...new Set(dup)].join(', ')}`);
  }

  // git に無い spec の宣言は数えない。手元でだけ緑になり、CI で初めて落ちるのを防ぐ。
  // 流れないテスト (skip / fixme) の宣言も数えない。そのルートは一度も開かれない
  const counted = declarations.filter((d) => tracked.has(d.file) && !d.skipped);
  for (const d of declarations) {
    if (!tracked.has(d.file)) problems.push(`${d.file}:${d.line}: git が追跡していない spec の宣言は数えない (git add する): ${d.route}`);
    else if (d.skipped) problems.push(`${d.file}:${d.line}: skip / fixme のテストの宣言は数えない (流れるテストに付ける): ${d.route}`);
  }
  if (counted.length === 0) untrusted.push('spec の宣言を 1 件も拾えなかった');

  /** @type {Map<string, Declaration[]>} */
  const byRoute = new Map();
  for (const d of counted) {
    if (!routeSet.has(d.route)) {
      problems.push(`${d.file}:${d.line}: 宣言したルートが router.definition.ts に無い: ${d.route}`);
      continue;
    }
    byRoute.set(d.route, [...(byRoute.get(d.route) ?? []), d]);
  }

  const allow = parseList(allowlist, ALLOWLIST);
  const pend = parseList(pending, PENDING);
  problems.push(...allow.problems, ...pend.problems);
  for (const row of pend.rows) {
    if (!/^#\d+/.test(row.note)) problems.push(`${PENDING}:${row.line}: 2 列目は付ける予定の issue の番号 (#123) で始める: ${row.note}`);
  }

  /** @type {Map<string, string>} */
  const listed = new Map();
  for (const [name, rows] of /** @type {const} */ ([[ALLOWLIST, allow.rows], [PENDING, pend.rows]])) {
    for (const row of rows) {
      const where = `${name}:${row.line}`;
      if (!routeSet.has(row.route)) {
        problems.push(`${where}: router.definition.ts に無いルート (消すか直す): ${row.route}`);
        continue;
      }
      if (listed.has(row.route)) {
        problems.push(`${where}: ${row.route} は ${listed.get(row.route)} にも載っている`);
        continue;
      }
      listed.set(row.route, where);
      const decl = byRoute.get(row.route);
      if (decl != null) {
        problems.push(`${where}: ${row.route} は spec が宣言している (${decl[0].file}:${decl[0].line})。この行を消す`);
      }
    }
  }

  for (const r of routes) {
    if (!byRoute.has(r) && !listed.has(r)) {
      problems.push(`${r}: どの spec も宣言していない。spec に coversRoutes('${r}') を付けるか、${PENDING} か ${ALLOWLIST} に載せる`);
    } else if (tablePrefix !== undefined && !listed.has(r) && !(byRoute.get(r) ?? []).some((d) => d.file.startsWith(tablePrefix))) {
      problems.push(`${r}: 全ルートの表 (fixtures/route_table.ts) に行が無い (表の spec ${tablePrefix}*.spec.ts が宣言していない)`);
    }
  }

  return {
    problems,
    untrusted,
    covered: byRoute.size,
    allowed: allow.rows.length,
    pending: pend.rows.length,
  };
}

/**
 * Picks the `.js` files that would shadow a `.ts` spec or fixture: playwright resolves
 * an extensionless import to a `.js` next to the `.ts` first (docs/playwright.md).
 *
 * @param {string[]} ignored paths under tests/playwright that git ignores
 * @returns {string[]}
 */
export function shadowingFiles(ignored) {
  return ignored.filter((f) => f.endsWith('.js') && !/\/(node_modules|playwright-report|test-results)\//.test(f));
}

/**
 * Turns the result of `check` into messages and an exit code.
 *
 * @param {ReturnType<typeof check>} result
 * @param {(line: string) => void} out
 * @returns {number}
 */
export function report(result, out) {
  if (result.untrusted.length > 0) {
    for (const p of result.untrusted) out(`route-coverage: 検査を信用できない: ${p}`);
    return 2;
  }
  if (result.problems.length > 0) {
    for (const p of result.problems) out(`route-coverage: ${p}`);
    return 1;
  }
  out(`route-coverage: ok (spec の宣言 ${result.covered}、許可リスト ${result.allowed}、保留 ${result.pending})`);
  return 0;
}

/**
 * Lists the paths under tests/playwright that git ignores.
 *
 * @returns {string[]}
 */
export function ignoredFiles() {
  const out = execFileSync('git', ['ls-files', '-z', '--others', '--ignored', '--exclude-standard', '--', 'tests/playwright'], { cwd: repoRoot, encoding: 'utf-8', maxBuffer: 256 * 1024 * 1024 });
  return out.split('\0').filter((f) => f !== '');
}

/**
 * Lists the spec files tracked by git.
 *
 * @returns {Set<string>}
 */
export function trackedSpecs() {
  const out = execFileSync('git', ['ls-files', '-z', '--', 'tests/playwright/specs'], { cwd: repoRoot, encoding: 'utf-8' });
  return new Set(out.split('\0').filter((f) => f !== ''));
}

/** Runs `playwright test --list --reporter=json` over every spec and parses the report. */
export function listTests() {
  const env = { ...process.env };
  // shard で絞った一覧にしない。`--list` は test.only があっても全テストを並べるが、実行では
  // only の 1 本しか流れない。--forbid-only で一覧ごと落とす (exit 2)
  delete env.PLAYWRIGHT_SHARD;
  // バックアップのスタックでだけ流す specs/backup/ も並べる (playwright.config.ts)。外すと、
  // そこに書いた test.only や宣言を、このゲートが見なくなる
  env.PLAYWRIGHT_STACK = 'all';
  const cli = join(here, 'node_modules', '@playwright', 'test', 'cli.js');
  const out = execFileSync(process.execPath, [cli, 'test', '--config', 'playwright.config.ts', '--list', '--forbid-only', '--reporter=json'], {
    cwd: here,
    env,
    encoding: 'utf-8',
    maxBuffer: 256 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'inherit'],
  });
  return JSON.parse(out);
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
      out(`route-coverage: 検査を信用できない: .ts を隠す .js がある (docs/playwright.md の「spec を書くときの注意」。消してから流す): ${shadows.join(', ')}`);
      return 2;
    }
    const routes = extractRoutes(readFileSync(join(repoRoot, ROUTER_DEFINITION), 'utf-8'));
    const declarations = collectDeclarations(listTests());
    result = check({
      routes,
      declarations,
      tracked: trackedSpecs(),
      allowlist: readFileSync(join(repoRoot, ALLOWLIST), 'utf-8'),
      pending: readFileSync(join(repoRoot, PENDING), 'utf-8'),
      tablePrefix: TABLE_SPEC_PREFIX,
    });
  } catch (err) {
    out(`route-coverage: 検査できなかった: ${err instanceof Error ? err.message : String(err)}`);
    return 2;
  }
  return report(result, out);
}

if (process.argv[1] != null && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href) {
  process.exit(run((line) => console.log(line)));
}
