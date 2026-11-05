/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 2 人目が mention / reply / renote / quote / reaction / pollEnded /
// receiveFollowRequest を起こすと、/my/notifications にそれぞれの種類の通知が、
// その中身 (誰が、どのノートに) で出ることを確かめる (#4046)。
//
// 通知の 1 件ごとに `data-testid="notification-<type>"` が付いている
// (MkStreamingNotificationsTimeline.vue)。種類と中身の組で 1 件ずつ取り、同じ
// 種類が余分に出ていないことも見る。判定はページの本体に絞る (画面が広いと
// 右のウィジェット列にも同じ通知が出る)。
//
// **pollEnded は最後に待つ。** Elythia は期限切れの投票を 60 秒ごとの走査で
// 拾う (internal/core/poll/expiry_worker.go。本家は期限ちょうどに走る遅延ジョブ)
// ので、届くまで最大で 1 分かかる。画面を開いたまま、ストリーミングで届くのを待つ。

import { expect, test } from '@playwright/test';
import { pollForNotification } from '../../../fixtures/notifications';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser, pageBody, postNote, runTag } from '../../../fixtures/ui_timeline';

test.describe('UI: each notification type renders with its content', () => {
  test.setTimeout(150_000);

  test('mention, reply, renote, quote, reaction, pollEnded and follow request', coversRoutes('/my/notifications'), async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwntv');
    const actor = await freshUser(request, 'pwnta');
    const requester = await freshUser(request, 'pwntq');
    const tag = runTag('nt');

    // 投票は先に作っておき、ほかを確かめている間に期限を迎えさせる
    const pollText = `${tag}-poll`;
    const pollNoteId = await postNote(request, actor, pollText, { poll: { choices: ['a', 'b'], expiredAfter: 1000 } });
    await apiOk(request, 'notes/polls/vote', { i: viewer.token, noteId: pollNoteId, choice: 0 });

    const target = `${tag}-viewer-note`;
    const targetId = await postNote(request, viewer, target);
    const mention = `@${viewer.username} ${tag}-mention`;
    const reply = `${tag}-reply`;
    const quote = `${tag}-quote`;
    await postNote(request, actor, mention);
    await postNote(request, actor, reply, { replyId: targetId });
    await postNote(request, actor, null, { renoteId: targetId });
    await postNote(request, actor, quote, { renoteId: targetId });
    await apiOk(request, 'notes/reactions/create', { i: actor.token, noteId: targetId, reaction: '👍' });
    await apiOk(request, 'i/update', { i: viewer.token, isLocked: true });
    await apiOk(request, 'following/create', { i: requester.token, userId: viewer.id });

    for (const type of ['mention', 'reply', 'renote', 'quote', 'reaction', 'receiveFollowRequest']) {
      await pollForNotification(request, viewer.token, (n) => n.type === type);
    }

    await uiSigninAs(page, request, baseURL!, viewer);
    await gotoRoute(page, baseURL!, '/my/notifications');
    const body = pageBody(page);
    const item = (type: string) => body.locator(`[data-testid="notification-${type}"]`);

    // mention / reply / quote はノートとして描かれる
    await expect(item('mention').filter({ hasText: `${tag}-mention` })).toHaveCount(1, { timeout: 20_000 });
    await expect(item('reply').filter({ hasText: reply })).toHaveCount(1);
    await expect(item('quote').filter({ hasText: quote }).filter({ hasText: target })).toHaveCount(1);
    // renote / reaction は、誰がどのノートに、を出す
    await expect(item('renote').filter({ hasText: actor.username }).filter({ hasText: target })).toHaveCount(1);
    await expect(item('reaction').filter({ hasText: actor.username }).filter({ hasText: target })).toHaveCount(1);
    await expect(item('receiveFollowRequest').filter({ hasText: requester.username }).filter({ hasText: 'Follow request received' })).toHaveCount(1);
    for (const type of ['mention', 'reply', 'renote', 'quote', 'reaction', 'receiveFollowRequest']) {
      await expect(item(type), `${type} の通知が 1 件でない`).toHaveCount(1);
    }

    await expect(item('pollEnded').filter({ hasText: 'Poll results have become available' }).filter({ hasText: pollText })).toHaveCount(1, { timeout: 90_000 });
  });
});
