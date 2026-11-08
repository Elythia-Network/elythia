/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// リバーシの対局を 2 つの画面で通す (#4072)。既存の reversi_game.spec.ts は設定の画面が出るまで。
//
// 対局は API の reversi/match を両者から呼んで作る (1 回目は招待で 204、2 回目で成立して game を返す)。
// 両者が設定の画面の「Ready」を押すと始まり、黒の番の人が盤のマスを押して石を置き、白の人が
// 投了する。準備完了と石を置く操作はストリーミングで送るので、結果は reversi/show-game で見る。
//
// マスは grid-template-columns を持つ枠の直下の要素で、並びの順が盤の位置 (y * 幅 + x) になる。
// 最初の 4 つの石の色の並びで、黒の置ける位置が 2 通りに分かれるので、候補を順に押して、記録が
// 増えたら止める。

import { type APIRequestContext, type Browser, expect, type Page, test } from '@playwright/test';
import { type Principal } from '../../../fixtures/auth';
import { signinWithState } from '../../../fixtures/settings_table';
import { clickButtonByText } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser } from '../../../fixtures/ui_timeline';

type Game = { id: string; user1Id: string; user2Id: string; black: number | null; isStarted: boolean; isEnded: boolean; logs: unknown[]; surrenderedUserId: string | null };

async function signedInPage(browser: Browser, request: APIRequestContext, baseURL: string, user: Principal): Promise<Page> {
  const context = await browser.newContext({ ignoreHTTPSErrors: true, locale: 'en-US' });
  const page = await context.newPage();
  await signinWithState(page, request, baseURL, user);
  return page;
}

// 8x8 の初期配置で黒が置ける位置。中央の 4 つの石の色の並びで、どちらかの組になる
const BLACK_FIRST_MOVES = [19, 26, 37, 44, 20, 29, 34, 43];

test.describe('UI: reversi', () => {
  test.setTimeout(90_000);

  test('both get ready, black puts a stone and white surrenders', coversRoutes('/reversi/g/:gameId'), async ({ browser, request, baseURL }) => {
    const a = await freshUser(request, 'pwrva');
    const b = await freshUser(request, 'pwrvb');
    const invited = await request.post(`${baseURL}/api/reversi/match`, { data: { i: a.token, userId: b.id }, failOnStatusCode: false });
    expect(invited.status(), '招待にならない (前提)').toBe(204);
    const game = await apiOk(request, 'reversi/match', { i: b.token, userId: a.id }) as Game;
    const show = async () => await apiOk(request, 'reversi/show-game', { i: a.token, gameId: game.id }) as Game;

    const pageA = await signedInPage(browser, request, baseURL!, a);
    const pageB = await signedInPage(browser, request, baseURL!, b);
    try {
      for (const p of [pageA, pageB]) await gotoRoute(p, baseURL!, `/reversi/g/${game.id}`);
      for (const p of [pageA, pageB]) await clickButtonByText(p, 'Ready');
      await expect.poll(async () => (await show()).isStarted, { message: '両者が準備完了にしても始まらない' }).toBe(true);

      const started = await show();
      const blackId = started.black === 1 ? started.user1Id : started.user2Id;
      const [blackPage, whitePage] = blackId === a.id ? [pageA, pageB] : [pageB, pageA];
      const whiteId = blackId === a.id ? b.id : a.id;

      // 設定の画面にも同じ形の盤 (64 マス) がある。対局の画面へ切り替わった (投了のボタンが出た) のを
      // 待ってから、盤を数える
      await expect(blackPage.locator('button').filter({ hasText: /^\s*Surrender\s*$/ }), '対局の画面に切り替わらない').toHaveCount(1, { timeout: 20_000 });
      const cells = blackPage.locator('div[style*="grid-template-columns"] > div');
      await expect(cells, '盤が 8x8 で出ない').toHaveCount(64, { timeout: 20_000 });
      let placed = false;
      for (const pos of BLACK_FIRST_MOVES) {
        await cells.nth(pos).click();
        placed = await expect.poll(async () => (await show()).logs.length, { timeout: 2_000 }).toBe(1).then(() => true, () => false);
        if (placed) break;
      }
      expect(placed, '黒の番に石を置けない').toBe(true);

      await clickButtonByText(whitePage, 'Surrender');
      const surrendered = whitePage.waitForResponse((r) => new URL(r.url()).pathname === '/api/reversi/surrender', { timeout: 15_000 });
      await whitePage.getByTestId('modal-dialog-ok').click();
      expect((await surrendered).ok(), 'reversi/surrender が失敗した').toBe(true);
      const ended = await show();
      expect([ended.isEnded, ended.surrenderedUserId], '投了で終わらない').toEqual([true, whiteId]);
    } finally {
      await pageA.context().close();
      await pageB.context().close();
    }
  });
});
