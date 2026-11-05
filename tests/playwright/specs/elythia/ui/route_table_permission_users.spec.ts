/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 全ルートの表 (fixtures/route_table.ts) のうち、権限の要るルートを、権限の無い利用者で
// 開いて確かめる (#4041)。
//
// - モデレーターのルート: ロールの無い利用者には not-found のページになる
// - チャットのルート: chatAvailability が unavailable のロールの利用者には not-found の
//   ページになる (router.definition.ts の chatPage)
//
// 本当の防御はサーバーの認可で、ここで見るのは「画面が出てしまわない」ことだけ。

import { readFileSync } from 'node:fs';
import { test } from '@playwright/test';
import { expectNotFoundPage, failFastAfterFailures, openAsOutsider } from '../../../fixtures/route_check';
import { type RouteSeed, seedRoutesOnce } from '../../../fixtures/route_seed';
import { ROUTE_TABLE } from '../../../fixtures/route_table';
import { type RootFixture, uiSigninAs } from '../../../fixtures/ui_auth';
import { coversRoutes } from '../../../fixtures/ui_route';

test.describe('UI: routes that need a permission, opened without it', () => {
  let seed: RouteSeed;

  failFastAfterFailures(test);

  test.beforeAll(async ({ playwright, baseURL }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    const request = await playwright.request.newContext({ ignoreHTTPSErrors: true });
    seed = await seedRoutesOnce(request, baseURL!, root);
    await request.dispose();
  });

  for (const entry of ROUTE_TABLE.filter((e) => e.kind === 'moderator' || e.kind === 'chat')) {
    const who = entry.kind === 'moderator' ? 'a user with no role' : 'a user who may not chat';
    test(`${entry.route} as ${who}`, coversRoutes(entry.route), async ({ page, baseURL, request }) => {
      await uiSigninAs(page, request, baseURL!, entry.kind === 'moderator' ? seed.outsider : seed.noChat);
      await openAsOutsider(page, baseURL!, entry.path(seed));
      await expectNotFoundPage(page, entry.route);
    });
  }
});
