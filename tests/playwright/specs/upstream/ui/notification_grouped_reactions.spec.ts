/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 同じノートへの 3 人のリアクションが、通知欄で 1 行 (`reaction:grouped`) に
// まとまることを確かめる (#4046)。
//
// まとめるのは `i/notifications-grouped` (まとめ表示は既定で有効。preferences の
// useGroupedNotifications)。まとまった行には人数と元のノートが出て、3 人の
// アバターが並ぶ。ばらの `reaction` の行は出ない。

import { expect, test } from '@playwright/test';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser, pageBody, postNote, runTag, waitForApiBody } from '../../../fixtures/ui_timeline';

test.describe('UI: grouped reaction notifications', () => {
  test.setTimeout(90_000);

  test('three reactions to the same note are grouped into one row', coversRoutes('/my/notifications'), async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwngv');
    const reactors = [await freshUser(request, 'pwngr'), await freshUser(request, 'pwngr'), await freshUser(request, 'pwngr')];
    const tag = runTag('ng');
    const target = `${tag}-viewer-note`;
    const targetId = await postNote(request, viewer, target);
    for (const [k, reactor] of reactors.entries()) {
      await apiOk(request, 'notes/reactions/create', { i: reactor.token, noteId: targetId, reaction: ['👍', '❤', '🎉'][k] });
    }
    // 通知の書き込みは非同期なので、3 件そろうまで待ってから開く
    await expect
      .poll(async () => {
        const list = (await apiOk(request, 'i/notifications', { i: viewer.token, includeTypes: ['reaction'], markAsRead: false })) as { type: string }[];
        return list.length;
      })
      .toBe(3);

    await uiSigninAs(page, request, baseURL!, viewer);
    const sent = waitForApiBody(page, 'i/notifications-grouped');
    await gotoRoute(page, baseURL!, '/my/notifications');
    await sent;

    const grouped = pageBody(page).locator('[data-testid="notification-reaction:grouped"]');
    await expect(grouped).toHaveCount(1, { timeout: 20_000 });
    await expect(grouped).toContainText('3 users reacted');
    await expect(grouped).toContainText(target);
    for (const reactor of reactors) {
      await expect(grouped.locator(`a[href="/@${reactor.username}"]`), `${reactor.username} のアバターが無い`).toHaveCount(1);
    }
    await expect(pageBody(page).locator('[data-testid="notification-reaction"]'), 'ばらのリアクションの行が出た').toHaveCount(0);
  });
});
