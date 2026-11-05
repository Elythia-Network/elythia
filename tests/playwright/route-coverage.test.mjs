/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// route-coverage.mjs のテスト (#4035)。`node --test tests/playwright/route-coverage.test.mjs`
// で流す (frontend.yml の frontend-lint job、make playwright-route-check)。
//
// 「ルートを足して spec を書かない」「宣言を消す」「リストに実在しないルートを書く」
// 「抽出が空振りする」の 4 つで落ちることを、小さな入力で確かめる。最後に、実際の
// router.definition.ts から下限を満たして拾えることを見る。

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import {
  ALLOWLIST,
  MIN_ROUTES,
  PENDING,
  REQUIRED_ROUTES,
  ROUTER_DEFINITION,
  check,
  collectDeclarations,
  extractRoutes,
  parseList,
  report,
  shadowingFiles,
} from './route-coverage.mjs';

const repoRoot = join(fileURLToPath(new URL('.', import.meta.url)), '..', '..');

const SOURCE = `
import { page } from './x';
export const ROUTE_DEF = [{
  path: '/',
  component: page(() => import('./a.vue')),
}, {
  path: '/notes/:noteId/:initialTab?',
  component: page(() => import('./b.vue')),
}, {
  path: \`/tags/:tag\`,
  component: page(() => import('./c.vue')),
}, {
  path: '/settings',
  children: [{
    path: '/profile',
    component: page(() => import('./d.vue')),
  }, {
    path: '/',
    component: page(() => import('./e.vue')),
  }],
}, {
  path: '/:(*)',
  component: page(() => import('./f.vue')),
}] as const satisfies RouteDef[];
`;

test('extractRoutes joins children to their parent and skips the child "/"', () => {
  assert.deepEqual(extractRoutes(SOURCE), ['/', '/notes/:noteId/:initialTab?', '/tags/:tag', '/settings', '/settings/profile', '/:(*)']);
});

test('extractRoutes joins every level of nested children', () => {
  const nested = SOURCE.replace("path: '/profile',", "path: '/profile',\n    children: [{ path: '/avatar', component: x }],");
  assert.ok(extractRoutes(nested).includes('/settings/profile/avatar'));
});

test('extractRoutes fails on children it cannot read instead of dropping them', () => {
  assert.throws(() => extractRoutes(SOURCE.replace("path: '/settings',", "path: '/settings',\n  children,")), /cannot be read/);
  assert.throws(() => extractRoutes(SOURCE.replace("path: '/settings',", "path: '/settings',\n  ...rest,")), /cannot be read/);
});

test('extractRoutes fails when ROUTE_DEF is renamed', () => {
  assert.throws(() => extractRoutes(SOURCE.replace('ROUTE_DEF', 'ROUTES')), /ROUTE_DEF was not found/);
});

test('extractRoutes fails on a path it cannot read', () => {
  assert.throws(() => extractRoutes(SOURCE.replace("path: '/'", "path: prefix + '/'")), /not a string literal/);
  assert.throws(() => extractRoutes(SOURCE.replace("path: '/:(*)',", '')), /has no path/);
});

/** A listing with one test per route, in the shape `playwright test --list --reporter=json` prints. */
function listing(routes) {
  return {
    suites: [{
      title: 'a.spec.ts',
      specs: [],
      suites: [{
        title: 'group',
        specs: routes.map((r, i) => ({
          file: 'ui/a.spec.ts',
          line: i + 1,
          title: `t${i}`,
          tests: [{ expectedStatus: 'passed', annotations: [{ type: 'route', description: r }, { type: 'issue', description: 'x' }] }],
        })),
      }],
    }],
  };
}

test('collectDeclarations reads only route annotations, nested suites included', () => {
  const d = collectDeclarations(listing(['/a', '/b']));
  assert.deepEqual(d.map((x) => [x.file, x.line, x.route]), [
    ['tests/playwright/specs/ui/a.spec.ts', 1, '/a'],
    ['tests/playwright/specs/ui/a.spec.ts', 2, '/b'],
  ]);
});

test('collectDeclarations fails on a listing without annotations', () => {
  assert.throws(() => collectDeclarations({}), /no suites/);
  const broken = listing(['/a']);
  delete broken.suites[0].suites[0].specs[0].tests[0].annotations;
  assert.throws(() => collectDeclarations(broken), /no annotations field/);
});

// 下限を満たす大きさのルート一覧を作る
const ROUTES = [...REQUIRED_ROUTES, ...Array.from({ length: MIN_ROUTES }, (_, i) => `/r${i}`)];
const TRACKED = new Set(['tests/playwright/specs/ui/a.spec.ts']);

/** The passing baseline: every route but the last two is declared; one is allowed, one is pending. */
function baseline() {
  const declared = ROUTES.slice(0, -2);
  return {
    routes: [...ROUTES],
    declarations: collectDeclarations(listing(declared)),
    tracked: TRACKED,
    allowlist: `# comment\n${ROUTES.at(-2)}\tredirect だけで描画しない\n`,
    pending: `\n${ROUTES.at(-1)}\t#4024(8)\n`,
    tablePrefix: 'tests/playwright/specs/ui/a',
  };
}

test('the baseline passes', () => {
  const r = check(baseline());
  assert.deepEqual(r.problems, []);
  assert.deepEqual(r.untrusted, []);
  assert.equal(r.covered, ROUTES.length - 2);
  assert.equal(r.allowed, 1);
  assert.equal(r.pending, 1);
});

test('(i) a new route without a spec fails', () => {
  const input = baseline();
  input.routes.push('/new-page');
  assert.match(check(input).problems.join('\n'), /\/new-page: どの spec も宣言していない/);
});

test('(ii) removing a declaration fails', () => {
  const input = baseline();
  input.declarations = input.declarations.filter((d) => d.route !== '/r3');
  assert.match(check(input).problems.join('\n'), /\/r3: どの spec も宣言していない/);
});

test('(iii) a list row for a route that does not exist fails', () => {
  for (const key of ['allowlist', 'pending']) {
    const input = baseline();
    input[key] += '/gone\t#4024(8)\n';
    assert.match(check(input).problems.join('\n'), /router\.definition\.ts に無いルート .*\/gone/, key);
  }
});

test('(iv) an empty extraction is untrusted, not a pass', () => {
  const input = baseline();
  input.routes = [];
  input.allowlist = '';
  input.pending = '';
  const r = check(input);
  assert.match(r.untrusted.join('\n'), /ルートが 0 個しか拾えなかった/);
  for (const req of REQUIRED_ROUTES) assert.match(r.untrusted.join('\n'), new RegExp(`目印のルート ${req.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}`));
});

test('no declaration at all is untrusted', () => {
  const input = baseline();
  input.declarations = [];
  assert.match(check(input).untrusted.join('\n'), /宣言を 1 件も拾えなかった/);
});

test('a declaration of a route that does not exist fails', () => {
  const input = baseline();
  input.declarations = collectDeclarations(listing([...ROUTES.slice(0, -2), '/typo']));
  assert.match(check(input).problems.join('\n'), /宣言したルートが router\.definition\.ts に無い: \/typo/);
});

test('a list row for a declared route fails, so the lists shrink as specs are added', () => {
  for (const key of ['allowlist', 'pending']) {
    const input = baseline();
    input[key] += '/r0\t#4024(8)\n';
    assert.match(check(input).problems.join('\n'), /\/r0 は spec が宣言している/, key);
  }
});

test('a route in both lists fails', () => {
  const input = baseline();
  input.pending += `${ROUTES.at(-2)}\t#4024(15)\n`;
  assert.match(check(input).problems.join('\n'), /にも載っている/);
});

test('a pending row must name an issue', () => {
  const input = baseline();
  input.pending = `${ROUTES.at(-1)}\tあとで\n`;
  assert.match(check(input).problems.join('\n'), /issue の番号/);
});

test('a malformed row fails', () => {
  const r = parseList('/a\n/b\tx\textra\n/c\t \n', ALLOWLIST);
  assert.equal(r.rows.length, 0);
  assert.equal(r.problems.length, 3);
});

test('a declaration in a file git does not track is not counted', () => {
  const input = baseline();
  input.tracked = new Set();
  const r = check(input);
  assert.match(r.problems.join('\n'), /git が追跡していない spec/);
  assert.match(r.untrusted.join('\n'), /宣言を 1 件も拾えなかった/);
});

test('the real router.definition.ts meets the lower bound', () => {
  const routes = extractRoutes(readFileSync(join(repoRoot, ROUTER_DEFINITION), 'utf-8'));
  assert.ok(routes.length >= MIN_ROUTES, `${routes.length} routes`);
  for (const r of REQUIRED_ROUTES) assert.ok(routes.includes(r), r);
  assert.equal(new Set(routes).size, routes.length);
});

test('the real lists parse', () => {
  for (const name of [ALLOWLIST, PENDING]) {
    assert.deepEqual(parseList(readFileSync(join(repoRoot, name), 'utf-8'), name).problems, [], name);
  }
});

test('a declaration on a skipped test is not counted', () => {
  const input = baseline();
  input.declarations = input.declarations.map((d) => (d.route === '/r5' ? { ...d, skipped: true } : d));
  const msg = check(input).problems.join('\n');
  assert.match(msg, /skip \/ fixme のテストの宣言は数えない.*\/r5/);
  assert.match(msg, /\/r5: どの spec も宣言していない/);
});

test('collectDeclarations marks tests that the listing reports as skipped', () => {
  const l = listing(['/a', '/b']);
  l.suites[0].suites[0].specs[1].tests[0].expectedStatus = 'skipped';
  assert.deepEqual(collectDeclarations(l).map((d) => [d.route, d.skipped]), [['/a', false], ['/b', true]]);
});

test('a path defined twice fails', () => {
  const input = baseline();
  input.routes.push('/r0');
  assert.match(check(input).problems.join('\n'), /同じパスが 2 回ある: \/r0/);
});

test('report maps untrusted to 2, problems to 1 and a pass to 0', () => {
  const lines = [];
  const out = (l) => lines.push(l);
  assert.equal(report({ problems: ['p'], untrusted: ['u'], covered: 0, allowed: 0, pending: 0 }, out), 2);
  assert.equal(report({ problems: ['p'], untrusted: [], covered: 0, allowed: 0, pending: 0 }, out), 1);
  assert.equal(report({ problems: [], untrusted: [], covered: 3, allowed: 1, pending: 2 }, out), 0);
  assert.match(lines.join('\n'), /検査を信用できない: u/);
  assert.match(lines.at(-1), /ok \(spec の宣言 3、許可リスト 1、保留 2\)/);
});

test('shadowingFiles picks stray .js files but not dependencies or reports', () => {
  assert.deepEqual(shadowingFiles([
    'tests/playwright/fixtures/ui_route.js',
    'tests/playwright/specs/upstream/ui/a.spec.js',
    'tests/playwright/node_modules/x/index.js',
    'tests/playwright/playwright-report/trace/a.js',
    'tests/playwright/test-results/a.js',
    'tests/playwright/results.json',
  ]), ['tests/playwright/fixtures/ui_route.js', 'tests/playwright/specs/upstream/ui/a.spec.js']);
});

test('a route that the route table spec does not declare fails, even if another spec does', () => {
  const input = baseline();
  input.declarations = input.declarations.map((d) => (d.route === '/r7' ? { ...d, file: 'tests/playwright/specs/ui/other.spec.ts' } : d));
  input.tracked = new Set([...TRACKED, 'tests/playwright/specs/ui/other.spec.ts']);
  assert.match(check(input).problems.join('\n'), /\/r7: 全ルートの表 .* に行が無い/);
  // 2 つのリストに載ったルートは、表に無くてよい
  assert.deepEqual(check(baseline()).problems, []);
});
