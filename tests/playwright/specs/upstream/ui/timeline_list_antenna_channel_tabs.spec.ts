/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// `/` のリスト・アンテナ・チャンネルのタブから選ぶと、そのタイムラインへ移り、
// そこに入るノートが出て、入らないノートが出ないことを確かめる (#4046)。
//
// 3 つのタブはどれも、押すとメニューを出し、選んだものの画面へ移る
// (timeline.vue の chooseList / chooseAntenna / chooseChannel)。移った先の画面が
// 対象の id で取得を送ったこと (UI が送った値) と、出たノートを見る。

import { expect, test, type APIRequestContext, type Page } from '@playwright/test';
import type { Principal } from '../../../fixtures/auth';
import { pollForTimelineNote } from '../../../fixtures/timeline';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { coversRoutes, expectRoutedPage } from '../../../fixtures/ui_route';
import { apiOk, clickMenuItem, clickPageTab, expectNotesInBody, freshUser, postNote, runTag, waitForApiBody } from '../../../fixtures/ui_timeline';

async function openHome(page: Page, request: APIRequestContext, baseURL: string, viewer: Principal): Promise<void> {
  await uiSigninAs(page, request, baseURL, viewer);
  await expectRoutedPage(page, '/');
}

test.describe('UI: list, antenna and channel tabs of the timeline', () => {
  test.setTimeout(90_000);

  test('the list tab opens the list timeline with only its members', coversRoutes('/timeline/list/:listId'), async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwtlav');
    const member = await freshUser(request, 'pwtlam');
    const outsider = await freshUser(request, 'pwtlao');
    const tag = runTag('tla');
    const list = await apiOk(request, 'users/lists/create', { i: viewer.token, name: `${tag}-list` });
    await apiOk(request, 'users/lists/push', { i: viewer.token, listId: list.id, userId: member.id });
    const inList = `${tag}-member`;
    const outOfList = `${tag}-outsider`;
    const inListId = await postNote(request, member, inList);
    await postNote(request, outsider, outOfList);
    // リストへの振り分けは非同期 (fanout)。開く前に反映を待つ
    await pollForTimelineNote(request, 'notes/user-list-timeline', viewer.token, inListId, { listId: list.id });

    await openHome(page, request, baseURL!, viewer);
    await clickPageTab(page, 'timeline-tab-lists');
    const sent = waitForApiBody(page, 'notes/user-list-timeline');
    await clickMenuItem(page, `${tag}-list`);
    expect(await sent).toMatchObject({ listId: list.id });
    await expect(page).toHaveURL(new RegExp(`/timeline/list/${list.id}$`));
    await expectNotesInBody(page, [inList], [outOfList]);
  });

  test('the antenna tab opens the antenna timeline with only matching notes', coversRoutes('/timeline/antenna/:antennaId'), async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwtlav');
    const author = await freshUser(request, 'pwtlaa');
    const tag = runTag('tla');
    const keyword = `${tag}kw`;
    const antenna = await apiOk(request, 'antennas/create', {
      i: viewer.token,
      name: `${tag}-antenna`,
      src: 'all',
      keywords: [[keyword]],
      excludeKeywords: [],
      users: [],
      caseSensitive: false,
      withReplies: false,
      withFile: false,
    });
    const matching = `${tag}-has ${keyword}`;
    const notMatching = `${tag}-without`;
    await postNote(request, author, matching);
    await postNote(request, author, notMatching);

    try {
      await openHome(page, request, baseURL!, viewer);
      await clickPageTab(page, 'timeline-tab-antennas');
      const sent = waitForApiBody(page, 'antennas/notes');
      await clickMenuItem(page, `${tag}-antenna`);
      expect(await sent).toMatchObject({ antennaId: antenna.id });
      await expect(page).toHaveURL(new RegExp(`/timeline/antenna/${antenna.id}$`));
      await expectNotesInBody(page, [matching], [notMatching]);
    } finally {
      // アンテナは利用者ごとの上限がある。作った人は使い捨てだが、行は残さない
      await apiOk(request, 'antennas/delete', { i: viewer.token, antennaId: antenna.id });
    }
  });

  test('the channel tab opens a favorited channel with only its notes', coversRoutes('/channels/:channelId'), async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwtlav');
    const author = await freshUser(request, 'pwtlac');
    const tag = runTag('tla');
    const channel = await apiOk(request, 'channels/create', { i: viewer.token, name: `${tag}-channel` });
    await apiOk(request, 'channels/favorite', { i: viewer.token, channelId: channel.id });
    const inChannel = `${tag}-in-channel`;
    const outOfChannel = `${tag}-out-of-channel`;
    await postNote(request, author, inChannel, { channelId: channel.id });
    await postNote(request, author, outOfChannel);

    await openHome(page, request, baseURL!, viewer);
    await clickPageTab(page, 'timeline-tab-channels');
    const sent = waitForApiBody(page, 'channels/timeline');
    await clickMenuItem(page, `${tag}-channel`);
    // お気に入りのチャンネルは、開くとタイムラインのタブになる (channel.vue)
    expect(await sent).toMatchObject({ channelId: channel.id });
    await expect(page).toHaveURL(new RegExp(`/channels/${channel.id}$`));
    await expectNotesInBody(page, [inChannel], [outOfChannel]);
  });
});
