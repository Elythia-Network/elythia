/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// タイムラインを開いたまま、2 人目が API で投稿すると、ストリーミングで画面に
// 差し込まれることを確かめる (#4046)。
//
// 差し込みがストリーミングで来たことは、開いた後にタイムラインの取得が 1 度も
// 送られていないことで見る (取り直しで出たのなら、ストリーミングは壊れていても
// 通ってしまう)。フォローしていない人の投稿は home に差し込まれない。その投稿を
// 先に、フォローしている人の投稿を後に出し、後者が出た時点で前者が無いことを見る。

import { expect, test } from '@playwright/test';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { expectRoutedPage } from '../../../fixtures/ui_route';
import { expectNotesInBody, follow, freshUser, pageBody, postNote, recordApiBodies, recordStreamChannels, runTag, waitForApiBody, waitForStreamChannel } from '../../../fixtures/ui_timeline';

test.describe('UI: timeline streaming', () => {
  test.setTimeout(90_000);

  test('a note posted by a followee is inserted without reloading', async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwtsv');
    const followee = await freshUser(request, 'pwtsf');
    const stranger = await freshUser(request, 'pwtss');
    await follow(request, viewer, followee);

    const tag = runTag('ts');
    const before = `${tag}-before`;
    await postNote(request, followee, before);

    const stream = recordStreamChannels(page);
    const firstFetch = waitForApiBody(page, 'notes/timeline');
    await uiSigninAs(page, request, baseURL!, viewer);
    await expectRoutedPage(page, '/');
    await firstFetch;
    await expectNotesInBody(page, [before], []);
    await waitForStreamChannel(stream, 'homeTimeline');

    // ここから先のタイムラインの取得を数える
    const refetches = recordApiBodies(page, 'notes/timeline');

    const fromStranger = `${tag}-stranger`;
    const fromFollowee = `${tag}-followee`;
    await postNote(request, stranger, fromStranger);
    await postNote(request, followee, fromFollowee);

    await expectNotesInBody(page, [fromFollowee], [fromStranger]);
    // 新しいものが上に入る
    const body = await pageBody(page).innerText();
    expect(body.indexOf(fromFollowee), '新着が既存のノートより上に入っていない').toBeLessThan(body.indexOf(before));
    expect(refetches, 'タイムラインを取り直して出した (ストリーミングで届いていない)').toEqual([]);
  });
});
