/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 全ルートの表 (fixtures/route_table.ts) を、未ログインで開いて確かめる (#4041)。
//
// - 公開のルート: not-found にならず、サインインのダイアログも出ず、ページの title が
//   付き (ページが mount した)、page error も出ない。ルートに loginRequired を書き足して
//   しまうと、ここで落ちる。未ログインで中身が変わるページがあるので、表の marker までは
//   求めない
// - ログイン必須 (チャットを含む): not-found のページに差し替わり、サインインの
//   ダイアログが出る。loginRequired を書き忘れると、ここで落ちる
// - モデレーター: not-found のページになる
// - リダイレクトだけのルートは、行き先の扱いで決まるので見ない
//
// 本当の防御はサーバーの認可で、ここで見るのは「画面が出てしまわない」ことだけ。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { collectPageErrors, expectNotFoundPage, expectSigninRequired, failFastAfterFailures, openAsOutsider, pageTitle, signinDialogCount } from '../../../fixtures/route_check';
import { type RouteSeed, seedRoutesOnce } from '../../../fixtures/route_seed';
import { ROUTE_TABLE } from '../../../fixtures/route_table';
import type { RootFixture } from '../../../fixtures/ui_auth';
import { coversRoutes, NOT_FOUND_TITLE } from '../../../fixtures/ui_route';

test.describe('UI: every route, signed out', () => {
  let seed: RouteSeed;

  failFastAfterFailures(test);

  test.beforeAll(async ({ playwright, baseURL }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    const request = await playwright.request.newContext({ ignoreHTTPSErrors: true });
    seed = await seedRoutesOnce(request, baseURL!, root);
    await request.dispose();
  });

  for (const entry of ROUTE_TABLE.filter((e) => e.kind !== 'redirect' && e.route !== '/:(*)')) {
    test(`${entry.route} signed out`, coversRoutes(entry.route), async ({ page, baseURL }) => {
      const errors = collectPageErrors(page);
      await openAsOutsider(page, baseURL!, entry.path(seed));
      switch (entry.kind) {
        case 'public': {
          const title = await pageTitle(page);
          expect(title, `${entry.route}: signed out, still opens`).not.toBe(NOT_FOUND_TITLE);
          expect(await signinDialogCount(page), `${entry.route}: no sign-in dialog`).toBe(0);
          // 表に書いた違い (signedOut) は、実際に起きていることも見る。直ったのに表に残ると、
          // その後に壊れても気付けない
          const differs = entry.signedOut ?? {};
          // title を付けないページ (/verify-email など) は表の marker に title が無い
          if (entry.marker?.(seed).title !== undefined) {
            const sets = (await page.title()).includes(' | ');
            expect(sets, `${entry.route}: signed out, the page ${differs.noTitle !== undefined ? `sets no title (${differs.noTitle})` : 'sets its title'}`).toBe(differs.noTitle === undefined);
          }
          if (differs.pageError === undefined) {
            expect(errors, `${entry.route}: page errors`).toEqual([]);
          } else {
            // 書いた理由のエラーだけが出ていること (別のエラーが混ざったら、それは回帰)
            expect(errors.length, `${entry.route}: signed out, a page error (${differs.pageError.reason})`).toBeGreaterThan(0);
            for (const e of errors) {
              expect(e, `${entry.route}: signed out, only the page error of the table (${differs.pageError.reason})`).toMatch(differs.pageError.match);
            }
          }
          break;
        }
        case 'login':
        case 'chat':
          await expectSigninRequired(page, entry.route);
          break;
        case 'moderator':
          await expectNotFoundPage(page, entry.route);
          break;
      }
    });
  }
});
