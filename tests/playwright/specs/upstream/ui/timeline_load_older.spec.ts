/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// タイムラインの下端まで読むと、`untilId` 付きの取得が送られ、古いノートが
// 出ることを確かめる (#4046)。
//
// 最初の取得は 15 件 (paginator.ts の FIRST_FETCH_LIMIT) なので、35 件を作ると
// 古い 20 件は最初は出ない。`untilId` は、最初に出た 15 件のうち最も古いものの
// id でなければならない (違う id だと、間が抜けるか重なる)。

import { expect, test } from '@playwright/test';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { expectRoutedPage } from '../../../fixtures/ui_route';
import { expectNotesInBody, freshUser, pageBody, postNote, runTag, scrollPageBodyToBottom, waitForApiBody } from '../../../fixtures/ui_timeline';

const SEEDED = 35;
const FIRST_PAGE = 15;

test.describe('UI: timeline loads older notes at the bottom', () => {
  test.setTimeout(120_000);

  test('scrolling to the bottom sends untilId and shows the oldest notes', async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwtlo');
    const tag = runTag('to');
    const ids: string[] = [];
    const texts: string[] = [];
    // ホームには自分のノートが入る。古い順に作る (texts[0] が最も古い)
    for (let n = 1; n <= SEEDED; n++) {
      const text = `${tag}-${String(n).padStart(2, '0')}`;
      texts.push(text);
      ids.push(await postNote(request, viewer, text));
    }

    const firstFetch = waitForApiBody(page, 'notes/timeline');
    await uiSigninAs(page, request, baseURL!, viewer);
    await expectRoutedPage(page, '/');
    expect(await firstFetch).not.toHaveProperty('untilId');

    const newest = texts.slice(-FIRST_PAGE);
    const oldestShown = ids[SEEDED - FIRST_PAGE];
    await expectNotesInBody(page, [newest[0], newest[newest.length - 1]], [texts[0], texts[SEEDED - FIRST_PAGE - 1]]);

    const older = waitForApiBody(page, 'notes/timeline', (b) => b.untilId !== undefined);
    await scrollPageBodyToBottom(page);
    expect(await older, '下端で送った取得の untilId が、出ていた最も古いノートの id でない').toMatchObject({ untilId: oldestShown });

    // 最も古いノートまで出る。遠いノートは DOM から外れるので、下端へ寄せながら待つ
    await expect
      .poll(
        async () => {
          await scrollPageBodyToBottom(page);
          return pageBody(page).getByText(texts[0], { exact: true }).count();
        },
        { message: '最も古いノートが出ない', timeout: 20_000 },
      )
      .toBe(1);
    await expect(pageBody(page).getByText(texts[1], { exact: true })).toHaveCount(1);
  });
});
