/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ミュート・ブロック・リノートのミュートが、ローカルタイムラインに効くことを
// 確かめる (#4046)。
//
// 効き方は本家のタイムラインの絞り込み (FanoutTimelineEndpointService) と同じ:
//   - ミュートした人のノートは出ない
//   - **自分をブロックしている人**のノートは出ない (自分がブロックした人の
//     ノートを落とす絞り込みは本家に無い)
//   - リノートをミュートした人は、本人のノートは出て、リノートだけが出ない
// リノートは元のノートの本文で描かれるので、元のノートの本文がいくつ出ているか
// (元の 1 つだけか、リノートの分も出たか) で見る。ほかに絞り込まれない人のノートを
// 前後に置き、それが出た時点で、間のものが出ていないことを見る。

import { expect, test } from '@playwright/test';
import { pollForTimelineNote } from '../../../fixtures/timeline';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { expectRoutedPage } from '../../../fixtures/ui_route';
import { apiOk, clickPageTab, expectNotesInBody, freshUser, pageBody, postNote, runTag, waitForApiBody } from '../../../fixtures/ui_timeline';

test.describe('UI: mute, block and renote mute on the timeline', () => {
  test.setTimeout(120_000);

  test('muted, blocking and renote-muted users are filtered from the local timeline', async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwmbv');
    const other = await freshUser(request, 'pwmbo');
    const muted = await freshUser(request, 'pwmbm');
    const blocker = await freshUser(request, 'pwmbb');
    const renoteMuted = await freshUser(request, 'pwmbr');

    await apiOk(request, 'mute/create', { i: viewer.token, userId: muted.id });
    await apiOk(request, 'blocking/create', { i: blocker.token, userId: viewer.id });
    await apiOk(request, 'renote-mute/create', { i: viewer.token, userId: renoteMuted.id });

    const tag = runTag('mb');
    const t = {
      first: `${tag}-other-first`,
      muted: `${tag}-muted`,
      blocker: `${tag}-blocker`,
      renoteMutedOwn: `${tag}-renote-muted-own`,
      last: `${tag}-other-last`,
    };
    const firstId = await postNote(request, other, t.first);
    await postNote(request, muted, t.muted);
    await postNote(request, blocker, t.blocker);
    await postNote(request, renoteMuted, t.renoteMutedOwn);
    // 本文なしのリノート (引用でないもの) だけが、リノートのミュートの対象
    await postNote(request, renoteMuted, null, { renoteId: firstId });
    const lastId = await postNote(request, other, t.last);
    await pollForTimelineNote(request, 'notes/local-timeline', viewer.token, lastId);

    await uiSigninAs(page, request, baseURL!, viewer);
    await expectRoutedPage(page, '/');
    const localSent = waitForApiBody(page, 'notes/local-timeline');
    await clickPageTab(page, 'timeline-tab-local');
    await localSent;

    await expectNotesInBody(page, [t.last, t.renoteMutedOwn, t.first], [t.muted, t.blocker]);
    // 元のノートの本文は 1 つだけ (リノートの分が出ていない)
    await expect(pageBody(page).getByText(t.first, { exact: true }), 'リノートのミュートが効いていない').toHaveCount(1);
  });
});
