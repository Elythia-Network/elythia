/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// アカウントの設定 (#4056) のうち、/settings/notifications の通知の受け取り
// (notificationRecieveConfig)。種類ごとに 1 行で、どれも「None」(never) にして、
// その種類の通知が届かないことを確かめる。既定は「All」なので、変異の確かめ
// (設定を変えない) では通知が届いて効き目の確かめが落ちる。
//
// **届かないことは、後から出す目印の通知が届くまで待ってから見る。** 目印は
// notifications/test-notification の 'test' 通知で、受信の設定の対象外。通知を作る
// 経路には裏で動くもの (サインインやトークンの発行) があるので、目印が届いた後も
// 少し待つ。

import { randomUUID } from 'node:crypto';
import { type APIRequestContext, expect, type Locator, type Page, test } from '@playwright/test';
import { DEFAULT_TEST_PASSWORD, type Principal, randomUsername, signin, signupUser } from '../../../../fixtures/auth';
import { pageBody } from '../../../../fixtures/route_check';
import { type AccountRow, expectControl, runAccountRow, setControl } from '../../../../fixtures/settings_table';
import { coversRoutes } from '../../../../fixtures/ui_route';
import { apiOk, follow, postNote, runTag } from '../../../../fixtures/ui_timeline';

/** The MkFolder (role="group") whose header has a part reading exactly `label`. */
async function folderGroup(page: Page, label: string): Promise<Locator> {
  const n = await pageBody(page).evaluate((body, text) => {
    for (const el of Array.from(body.querySelectorAll('[data-pw-folder]'))) el.removeAttribute('data-pw-folder');
    const headers = Array.from(body.querySelectorAll('[data-testid="folder-header"]'))
      .filter((h) => Array.from(h.querySelectorAll('*')).some((d) => (d.textContent ?? '').trim() === text));
    if (headers.length !== 1) return headers.length;
    headers[0].closest('[role="group"]')?.setAttribute('data-pw-folder', '');
    return 1;
  }, label);
  expect(n, `見出しが「${label}」のフォルダ (ちょうど 1 つ)`).toBe(1);
  return pageBody(page).locator('[data-pw-folder]');
}

interface Notification {
  type: string;
  userId?: string;
  achievement?: string;
}

function notifications(request: APIRequestContext, user: Principal): Promise<Notification[]> {
  return apiOk(request, 'i/notifications', { i: user.token, limit: 100 });
}

/** Asserts that no notification `match` accepts reached `user`, after a marker notification did. */
async function expectNoNotification(request: APIRequestContext, user: Principal, match: (n: Notification) => boolean, message = '受け取らない種類の通知が届いた'): Promise<void> {
  await apiOk(request, 'notifications/test-notification', { i: user.token });
  await expect.poll(async () => (await notifications(request, user)).some((n) => n.type === 'test'), { message: '目印の通知が届かない', timeout: 10_000 }).toBe(true);
  await new Promise((resolve) => setTimeout(resolve, 2_000));
  expect((await notifications(request, user)).filter(match), message).toEqual([]);
}

async function other(request: APIRequestContext): Promise<Principal> {
  return signupUser(request, randomUsername('pwnotif'));
}

/** A row that sets the receive config of `type` to "None" and checks with `effect`. */
function neverRow(type: string, label: string, effect: AccountRow['effect'], knownBug?: string): AccountRow {
  return {
    name: `notificationRecieveConfig.${type} → never`, route: '/settings/notifications',
    set: async ({ page }) => {
      const group = await folderGroup(page, label);
      await setControl(page, group, { kind: 'select', label: 'None' });
      // アイコンの字形 (::before) が名前に入りうるので、role の名前ではなく文字で取る
      await group.locator('button').filter({ hasText: /^\s*Save\s*$/ }).click();
    },
    read: (me) => me.notificationRecieveConfig?.[type]?.type, expected: 'never',
    shows: async ({ page }) => {
      await expectControl(await folderGroup(page, label), { kind: 'select', label: 'None' });
    },
    effect,
    ...(knownBug ? { knownBug } : {}),
  };
}

test.describe('settings: account (notifications)', () => {
  test.setTimeout(60_000);
  const ROWS: AccountRow[] = [
    neverRow('follow', 'New followers', async ({ request, user }) => {
      const b = await other(request);
      await follow(request, b, user);
      await expectNoNotification(request, user, (n) => n.type === 'follow' && n.userId === b.id);
    }),
    neverRow('mention', 'Mentions', async ({ request, user }) => {
      const b = await other(request);
      await postNote(request, b, `@${user.username} ${runTag('pwmention')}`);
      await expectNoNotification(request, user, (n) => n.type === 'mention' && n.userId === b.id);
    }),
    neverRow('reply', 'Replies', async ({ request, user }) => {
      const b = await other(request);
      const noteId = await postNote(request, user, runTag('pwreplied'));
      await postNote(request, b, runTag('pwreply'), { replyId: noteId });
      await expectNoNotification(request, user, (n) => n.type === 'reply' && n.userId === b.id);
    }),
    neverRow('renote', 'Renotes', async ({ request, user }) => {
      const b = await other(request);
      const noteId = await postNote(request, user, runTag('pwrenoted'));
      await postNote(request, b, null, { renoteId: noteId });
      await expectNoNotification(request, user, (n) => n.type === 'renote' && n.userId === b.id);
    }),
    neverRow('quote', 'Quotes', async ({ request, user }) => {
      const b = await other(request);
      const noteId = await postNote(request, user, runTag('pwquoted'));
      await postNote(request, b, runTag('pwquote'), { renoteId: noteId });
      await expectNoNotification(request, user, (n) => n.type === 'quote' && n.userId === b.id);
    }),
    neverRow('reaction', 'Reactions', async ({ request, user }) => {
      const b = await other(request);
      const noteId = await postNote(request, user, runTag('pwreacted'));
      await apiOk(request, 'notes/reactions/create', { i: b.token, noteId, reaction: '👍' });
      await expectNoNotification(request, user, (n) => n.type === 'reaction' && n.userId === b.id);
    }),
    neverRow('pollEnded', 'Polls ending', {
      skip: '投票の締切を待つワーカー (internal/core/poll/expiry_worker.go、60 秒周期) から届く。行の時間内に締切を迎えられない。受信の設定は、通知を作る側 (notification.Service) が見る (#4058)',
    }),
    neverRow('scheduledNotePosted', 'Scheduled note was successful', {
      skip: '予約投稿のジョブ (internal/queue/processors/post_scheduled_note.go) から届き、予約の時刻を待つ必要がある。受信の設定は、通知を作る側 (notification.Service) が見る (#4058)',
    }),
    neverRow('scheduledNotePostFailed', 'Scheduled note failed', {
      skip: '予約投稿のジョブが失敗したときに届く (post_scheduled_note.go)。失敗を起こす手段が無い。受信の設定は、通知を作る側 (notification.Service) が見る (#4058)',
    }),
    neverRow('receiveFollowRequest', 'Received follow requests', async ({ request, user }) => {
      // フォローがリクエストになるよう、承認制にしておく
      await apiOk(request, 'i/update', { i: user.token, isLocked: true });
      const b = await other(request);
      await apiOk(request, 'following/create', { i: b.token, userId: user.id });
      await expectNoNotification(request, user, (n) => n.type === 'receiveFollowRequest' && n.userId === b.id);
    }),
    neverRow('app', 'Notifications from linked apps', async ({ request, user }) => {
      await apiOk(request, 'notifications/create', { i: user.token, body: runTag('pwapp'), header: 'pw' });
      await expectNoNotification(request, user, (n) => n.type === 'app');
    }),
    neverRow('chatRoomInvitationReceived', 'Invited to chat room', async ({ request, user }) => {
      const b = await other(request);
      const room = await apiOk(request, 'chat/rooms/create', { i: b.token, name: runTag('pwroom') });
      await apiOk(request, 'chat/rooms/invitations/create', { i: b.token, roomId: room.id, userId: user.id });
      await expectNoNotification(request, user, (n) => n.type === 'chatRoomInvitationReceived' && n.userId === b.id);
    }),
    neverRow('achievementEarned', 'Achievement unlocked', async ({ request, user }) => {
      // 画面が自分で取る実績もあるので、この行で取る実績の名前で絞る
      await apiOk(request, 'i/claim-achievement', { i: user.token, name: 'clickedClickHere' });
      await expectNoNotification(request, user, (n) => n.type === 'achievementEarned' && n.achievement === 'clickedClickHere');
    }),
    neverRow('login', 'Sign In', async ({ request, user }) => {
      await signin(request, user.username, DEFAULT_TEST_PASSWORD);
      await expectNoNotification(request, user, (n) => n.type === 'login');
    }),
    neverRow('createToken', 'Create access token', async ({ request, user }) => {
      // MiAuth の許可と同じ発行 (internal/api/auth/handler.go の miauth/gen-token)
      await apiOk(request, 'miauth/gen-token', { i: user.token, session: randomUUID(), name: runTag('pwtoken'), permission: ['read:account'] });
      await expectNoNotification(request, user, (n) => n.type === 'createToken');
    }),
  ];
  for (const row of ROWS) test(row.name, coversRoutes('/settings/notifications'), ({ page, request, baseURL }) => runAccountRow({ page, request, baseURL }, row));
});
