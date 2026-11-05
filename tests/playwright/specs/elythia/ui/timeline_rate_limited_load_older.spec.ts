/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 追い読みが 429 を受けたら、自動では繰り返さず、理由と再試行のボタンを出す
// ことを確かめる (Elythia 独自、#2955)。
//
// サーバーのレート制限は、拒否したリクエストも数えるので、429 のまま叩き続けると
// 解けない。本家は失敗を握り潰し、下端が見えるたびに追い読みを撃ち直す。
//
// 429 は `page.route` で返す (このスタックはレート制限を切ってある。instance.yml)。
// 「繰り返さない」は、下端へ何度も寄せて (自動の追い読みが撃たれる操作) 通信が
// 止んだ後も、`untilId` 付きの取得が 1 本のままであることで見る。最後に「再試行」を
// 押すと 1 本だけ増えて古いノートが出ることで、数え方が空振りしていないことも見る。

import { expect, test } from '@playwright/test';
import { isTsBackend } from '../../../fixtures/backend';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { expectRoutedPage } from '../../../fixtures/ui_route';
import { clickBodyButton, expectNotesInBody, freshUser, pageBody, postNote, runTag, scrollPageBodyToBottom, scrollPageBodyToTop } from '../../../fixtures/ui_timeline';

const SEEDED = 20;

test.describe('UI: timeline stops loading older notes on 429', () => {
  test.skip(isTsBackend, '429 で追い読みを止めるのは Elythia 独自 (#2955)');
  test.setTimeout(90_000);

  test('a 429 on the older page is shown and not retried automatically', async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwrl');
    const tag = runTag('rl');
    const texts: string[] = [];
    for (let n = 1; n <= SEEDED; n++) {
      const text = `${tag}-${String(n).padStart(2, '0')}`;
      texts.push(text);
      await postNote(request, viewer, text);
    }

    let limited = true;
    const olderRequests: unknown[] = [];
    await page.route('**/api/notes/timeline', async (route) => {
      const body = route.request().postDataJSON() as { untilId?: string } | null;
      if (body?.untilId === undefined) return route.continue();
      olderRequests.push(body);
      if (!limited) return route.continue();
      return route.fulfill({
        status: 429,
        contentType: 'application/json',
        headers: { 'Retry-After': '60' },
        body: JSON.stringify({
          error: {
            message: 'Rate limit exceeded. Please try again later.',
            code: 'RATE_LIMIT_EXCEEDED',
            id: 'd5826d14-3982-4d2e-8011-b9e9f02499ef',
          },
        }),
      });
    });

    await uiSigninAs(page, request, baseURL!, viewer);
    await expectRoutedPage(page, '/');
    await expectNotesInBody(page, [texts[SEEDED - 1]], [texts[0]]);

    await scrollPageBodyToBottom(page);
    await expect(pageBody(page).getByText('Rate limit exceeded', { exact: true }), '429 の理由が出ない').toBeVisible({ timeout: 20_000 });
    expect(olderRequests).toHaveLength(1);
    await expect(pageBody(page).getByRole('button', { name: 'Load more' }), '「もっと見る」が残っている').toHaveCount(0);

    // 下端が見えるたびに撃つ作りなら、ここで増える。スクロールするのは本体の中の要素
    // (window ではない) なので、本体を上に戻してから下端へ寄せ直す。撃たれないことは
    // 起きるのを待てないので、下端が見えるたびに 1 秒ずつ見張る
    for (let k = 0; k < 3; k++) {
      await scrollPageBodyToTop(page);
      await scrollPageBodyToBottom(page);
      await page.waitForTimeout(1_000);
    }
    expect(olderRequests, '429 の後に追い読みを自動で繰り返した').toHaveLength(1);
    await expect(pageBody(page).getByText(texts[0], { exact: true })).toHaveCount(0);

    // 再試行は人が押したときだけ。押すと 429 のページを取り直し、通れば古いノートが出る
    limited = false;
    await clickBodyButton(page, 'Retry');
    await expect
      .poll(
        async () => {
          await scrollPageBodyToBottom(page);
          return pageBody(page).getByText(texts[0], { exact: true }).count();
        },
        { message: '再試行の後も最も古いノートが出ない', timeout: 20_000 },
      )
      .toBe(1);
    // 再試行の 1 本は、429 になったページと同じ untilId で撃つ。通った後は普通の追い読みに
    // 戻るので、さらに古いページの取得が続くのはよい (本数は数えない)
    expect(olderRequests.length).toBeGreaterThanOrEqual(2);
    expect(olderRequests[1], '再試行が 429 のページと違うページを取りに行った').toEqual(olderRequests[0]);
    await expect(pageBody(page).getByText('Rate limit exceeded', { exact: true })).toHaveCount(0);
  });
});
