/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 全ルートの表 (fixtures/route_table.ts) の「admin」の行を、開ける人で開いて確かめる (#4041)。
// 確かめる中身は fixtures/route_check.ts の expectRouteOpens。表を領域ごとのファイルに
// 分けるのは、shard の割り振り (#4026) がファイル単位だから。

import { readFileSync } from 'node:fs';
import { test } from '@playwright/test';
import { expectRouteOpens, failFastAfterFailures } from '../../../fixtures/route_check';
import { type RouteSeed, seedRoutesOnce } from '../../../fixtures/route_seed';
import { ROUTE_TABLE } from '../../../fixtures/route_table';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { coversRoutes } from '../../../fixtures/ui_route';

test.describe('UI: every route opens (admin)', () => {
  let root: RootFixture;
  let seed: RouteSeed;

  failFastAfterFailures(test);

  test.beforeAll(async ({ playwright, baseURL }) => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    const request = await playwright.request.newContext({ ignoreHTTPSErrors: true });
    seed = await seedRoutesOnce(request, baseURL!, root);
    await request.dispose();
  });

  for (const entry of ROUTE_TABLE.filter((e) => e.area === 'admin')) {
    test(`${entry.route} opens`, coversRoutes(entry.route), async ({ page, baseURL }) => {
      // 公開のルートは未ログインでも開けることを、権限の表の spec が見る。ここでは
      // root で開く (root が持つデータを seed したので)
      await uiSigninAsRoot(page, baseURL!, root);
      await expectRouteOpens(page, baseURL!, entry, seed);
    });
  }
});
