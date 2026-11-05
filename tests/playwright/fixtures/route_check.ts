/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 全ルートの表 (route_table.ts) の 1 行を開いて確かめる (#4041)。
//
// 開いたらページが mount するのを待ってから判定する (待ち方は ui_route.ts の
// expectRoutedPage と同じ。待たずに判定すると、not-found の部品が出る前なので通る)。

import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { basename, join } from 'node:path';
import { expect, type Page, type Response, type TestType } from '@playwright/test';
import { NOT_FOUND_TITLE } from './ui_route';
import type { RouteEntry, RouteMarker } from './route_table';
import type { RouteSeed } from './route_seed';

/**
 * Page errors that every page raises in the test stack, with why.
 *
 * 手元と CI の stack は nginx の自己署名の証明書で HTTPS を張るので、Service Worker の
 * 登録が証明書のエラーで失敗する (本番では起きない)。
 */
export const KNOWN_PAGE_ERRORS: RegExp[] = [
  /^Failed to register a ServiceWorker for scope .*An SSL certificate error occurred when fetching the script\.$/,
];

/** The width of a narrow viewport, below which /settings and /admin show their own menu. */
export const NARROW_VIEWPORT = { width: 500, height: 900 };

/** Collects the page errors that are not known to every page. */
export function collectPageErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on('pageerror', (e) => {
    if (!KNOWN_PAGE_ERRORS.some((re) => re.test(e.message))) errors.push(e.message);
  });
  return errors;
}

/** Waits for the app to mount the routed page (the splash is gone and the network is idle). */
export async function waitForMountedPage(page: Page, timeout = 20_000): Promise<void> {
  await page.waitForFunction(() => document.getElementById('splash') === null, undefined, { timeout });
  await page.waitForLoadState('networkidle', { timeout });
}

/** The innermost router view: the body of the routed page, without the navigation and the widgets. */
export function pageBody(page: Page) {
  return page.locator('._pageContainer').last();
}

/** The part of the title the page sets, without " | <instance name>". */
export async function pageTitle(page: Page): Promise<string> {
  const title = await page.title();
  const i = title.lastIndexOf(' | ');
  return i < 0 ? title : title.slice(0, i);
}

/**
 * Waits for the page to call /api/<api> and get a 2xx. The rejection is
 * handled at once, so that a check that fails earlier does not leave it unhandled.
 */
function apiResponse(page: Page, api: string): Promise<Response> {
  const p = page.waitForResponse((r) => new URL(r.url()).pathname === `/api/${api}`, { timeout: 20_000 });
  p.catch(() => {});
  return p;
}

/** Checks every field of the marker on the page that has just mounted. */
export async function expectMarker(page: Page, route: string, marker: RouteMarker, api: Promise<Response> | null): Promise<void> {
  if (marker.title !== undefined) {
    expect(await pageTitle(page), `${route}: title`).toBe(marker.title);
  }
  if (marker.text !== undefined) {
    await expect(pageBody(page), `${route}: text in the page body`).toContainText(marker.text, { useInnerText: true });
  }
  if (marker.testId !== undefined) {
    // 中身が空だと高さが 0 で「見えない」になるページがある (/settings/profiles)。あることで見る
    await expect(page.getByTestId(marker.testId).first(), `${route}: data-testid`).toBeAttached();
  }
  if (api !== null) {
    const resp = await api.catch((cause: unknown) => {
      throw new Error(`${route}: the page did not call /api/${marker.api}`, { cause });
    });
    // 呼んだだけでは、API が失敗して画面がエラーを出していても通る
    expect(resp.ok(), `${route}: /api/${marker.api} answered ${resp.status()}`).toBe(true);
  }
}

/**
 * Opens the route as a viewer who may open it (the page is signed in as root
 * already, or signed out for a public route) and checks it: a redirect moves
 * to its target, any other route mounts its own page and shows its marker,
 * with no page error.
 */
export async function expectRouteOpens(page: Page, baseURL: string, entry: RouteEntry, seed: RouteSeed): Promise<void> {
  const errors = collectPageErrors(page);
  if (entry.narrow) await page.setViewportSize(NARROW_VIEWPORT);
  const marker = entry.marker?.(seed);
  // 前のページ (サインインで開いた /) の遅れたリクエストで満たされないよう、移った後で
  // 待ち始める。API は script が動いてから呼ばれるので、commit の後なら取りこぼさない
  const resp = await page.goto(`${baseURL}${entry.path(seed)}`, { waitUntil: 'commit' });
  const api = marker?.api !== undefined ? apiResponse(page, marker.api) : null;
  expect(resp?.status(), `${entry.route}: status`).toBe(200);
  await waitForMountedPage(page);

  if (entry.kind === 'redirect') {
    expect(entry.redirectTo, `${entry.route}: a redirect needs redirectTo`).toBeDefined();
    await expect(page, `${entry.route}: moves to its target`).toHaveURL((u) => u.pathname + u.hash === entry.redirectTo!(seed));
    expect(await pageTitle(page), `${entry.route}: the target is not the not-found page`).not.toBe(NOT_FOUND_TITLE);
  } else {
    expect(marker, `${entry.route}: needs a marker`).toBeDefined();
    if (entry.route !== '/:(*)') {
      expect(await pageTitle(page), `${entry.route}: not the not-found page`).not.toBe(NOT_FOUND_TITLE);
    }
    // 開いたパスに留まる (別のページへ移っていたら、そのページを見ていることになる)
    const want = new URL(`${baseURL}${entry.path(seed)}`).pathname;
    expect(new URL(page.url()).pathname, `${entry.route}: stays on the path`).toBe(want);
    await expectMarker(page, entry.route, marker!, api);
  }
  expect(errors, `${entry.route}: page errors`).toEqual([]);
}

/** Opens the path and waits for the routed page, for a viewer who may not open it. */
export async function openAsOutsider(page: Page, baseURL: string, path: string): Promise<void> {
  const resp = await page.goto(`${baseURL}${path}`, { waitUntil: 'domcontentloaded' });
  expect(resp?.status(), `${path}: status`).toBe(200);
  await waitForMountedPage(page);
}

/**
 * Counts the sign-in forms outside the page body, which is where the sign-in
 * dialog is. A page may embed the form itself (/auth/:token), which is no dialog.
 */
export function signinDialogCount(page: Page): Promise<number> {
  return page.evaluate(() => [...document.querySelectorAll('[data-testid="signin-page-input"]')]
    .filter((e) => e.closest('._pageContainer') === null).length);
}

/** Checks that a signed-out viewer of a loginRequired route gets the not-found page and the sign-in dialog. */
export async function expectSigninRequired(page: Page, route: string): Promise<void> {
  expect(await pageTitle(page), `${route}: signed out, the page is replaced with not-found`).toBe(NOT_FOUND_TITLE);
  await expect.poll(() => signinDialogCount(page), { message: `${route}: signed out, the sign-in dialog opens` }).toBe(1);
}

/** Checks that the viewer gets the not-found page and no sign-in dialog. */
export async function expectNotFoundPage(page: Page, route: string): Promise<void> {
  expect(await pageTitle(page), `${route}: the not-found page`).toBe(NOT_FOUND_TITLE);
  expect(await signinDialogCount(page), `${route}: no sign-in dialog`).toBe(0);
}

/** How many failed tests make the rest of a route table spec fail at once. */
export const FAIL_FAST_AFTER = 5;

/** Where failFastAfterFailures counts the failures of each spec file in a run. globalSetup clears it. */
export const FAIL_COUNT_DIR = '.auth/route-table-failures';

function failCountPath(file: string): string {
  return join(FAIL_COUNT_DIR, basename(file));
}

function readFailCount(file: string): number {
  try {
    return Number(readFileSync(failCountPath(file), 'utf-8')) || 0;
  } catch {
    return 0;
  }
}

/**
 * Makes the rest of the file fail at once after FAIL_FAST_AFTER failures.
 *
 * stack が壊れているなど全ルートが同じ理由で落ちると、1 件 30 秒の時間切れと retry で
 * CI の job の時間 (playwright.yml の timeout_minutes) を超え、報告も artifact も残らない
 * まま打ち切られる。skip ではなく落とすので、結果は赤のまま残る。
 *
 * **数はファイルに置く。** Playwright はテストが落ちるたびに worker を作り直すので、
 * モジュールの変数では毎回 0 に戻り、一度も打ち切らない (実際にそうなっていた)。
 */
export function failFastAfterFailures(t: TestType<any, any>): void {
  t.beforeEach(({}, info) => {
    const failures = readFailCount(info.file);
    if (failures >= FAIL_FAST_AFTER) {
      throw new Error(`${failures} tests of this file failed before; not opening more routes (see the first failures)`);
    }
  });
  t.afterEach(({}, info) => {
    if (info.status === info.expectedStatus) return;
    mkdirSync(FAIL_COUNT_DIR, { recursive: true });
    writeFileSync(failCountPath(info.file), String(readFailCount(info.file) + 1));
  });
}
