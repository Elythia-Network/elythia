/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// expectRoutedPage (fixtures/ui_route.ts) が not-found のページを本当に落とすかを
// 確かめる (#4025)。この判定が空振りすると、ルートを開くだけの spec は存在しない
// パスでも緑になる (実際に 18 パスでそうなっていた)。
//
// 存在しないパスでは落ちること (陰性の対照) と、実在するパスでは通ること (陽性の
// 対照) の両方を見る。片方だけだと、常に落ちる / 常に通る判定を見逃す。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture } from '../../../fixtures/ui_auth';
import { coversRoutes, expectRoutedPage, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: not-found guard for route specs', () => {
  test.setTimeout(60_000);

  test('a path with no route fails expectRoutedPage', coversRoutes('/:(*)'), async ({ page, baseURL }) => {
    const path = '/no-such-route-4025';
    await page.goto(`${baseURL}${path}`, { waitUntil: 'domcontentloaded' });
    await expect(expectRoutedPage(page, path)).rejects.toThrow(/not-found/);
  });

  // 権限の無い人が管理画面を開くと、router は not-found の部品を出す
  // (router.definition.ts。モデレーターでなければ not-found.vue)
  test('/admin while signed out fails expectRoutedPage', async ({ page, baseURL }) => {
    await page.goto(`${baseURL}/admin`, { waitUntil: 'domcontentloaded' });
    await expect(expectRoutedPage(page, '/admin')).rejects.toThrow(/not-found/);
  });

  // ルートを開く spec は gotoRoute を通す。gotoRoute が判定を呼び忘れると、
  // 上の 2 本は緑のまま、ルートの spec だけが素通りになる (#4026)
  test('a path with no route fails gotoRoute', async ({ page, baseURL }) => {
    await expect(gotoRoute(page, baseURL!, '/no-such-route-4026')).rejects.toThrow(/not-found/);
  });

  // SPA の外のパス (ドライブのファイル配信) は 404 を返す。gotoRoute が status を
  // 見なくなると、ここが通ってしまう
  test('a path served with a non-200 status fails gotoRoute', async ({ page, baseURL }) => {
    await expect(gotoRoute(page, baseURL!, '/files/no-such-file-4026')).rejects.toThrow(/should return 200/);
  });

  test('a real route passes gotoRoute', async ({ page, baseURL }) => {
    await gotoRoute(page, baseURL!, '/about');
  });

  test('a real route passes expectRoutedPage', async ({ page, baseURL }) => {
    await page.goto(`${baseURL}/about`, { waitUntil: 'domcontentloaded' });
    await expectRoutedPage(page, '/about');
  });

  // サーバーが title を描くルート (internal/server/ssr_meta.go)。title の形だけを
  // 待つ判定は、ここで mount の前に通ってしまっていた (#4025 のレビュー)
  test('a server-rendered route passes expectRoutedPage', async ({ page, baseURL }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    await page.goto(`${baseURL}/@${root.username}`, { waitUntil: 'domcontentloaded' });
    await expectRoutedPage(page, `/@${root.username}`);
  });
});
