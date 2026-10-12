/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #823 Phase 2 notification spec: mark-all-as-read + unreadNotificationsCount。
//
// upstream Misskey TS と mk-go は両方とも:
//   - notification 受信で /api/i の `unreadNotificationsCount` / `hasUnreadNotification`
//     が増える (WS notification event 経路で hook 起動)
//   - /api/notifications/mark-all-as-read (204) を呼ぶと両 field がゼロ化される
//
// 本 spec は両 backend 共通で:
//   1. user A + user B signup
//   2. user A が main channel WS subscribe
//   3. user B が user A を follow → notification 生成 (= unread に積まれる)
//   4. WS で notification event を受信 (= notification 確定の barrier)
//   5. /api/i で unreadNotificationsCount >= 1 / hasUnreadNotification = true
//   6. /api/notifications/mark-all-as-read を呼ぶ
//   7. /api/i で unreadNotificationsCount === 0 / hasUnreadNotification = false
//
// 注: /api/i/notifications は default `markAsRead: true` で list 取得自体が
// 既読化の副作用を持つ (#420)。本 spec で間に挟むと mark-all-as-read 単体
// の効果が検証できなくなるため、unread 状態の確認は /api/i のみで行う。

import { expect, test, type APIRequestContext } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { type NotificationBody } from '../../../../fixtures/notifications';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import {
  awaitChannelEvent,
  awaitSubscribeBuffer,
  openStream,
  subscribeChannel,
} from '../../../../fixtures/streaming';

interface UnreadFields {
  unreadNotificationsCount: number;
  hasUnreadNotification: boolean;
}

// /api/i の unread 系 field を取得して predicate を満たすまで polling する。
// notification の write は queue 経由で async なので、生成直後は count が
// 0 のままであることがある。expect.poll で 5s 範囲を retry する。
async function pollUnread(
  request: APIRequestContext,
  token: string,
  predicate: (u: UnreadFields) => boolean,
): Promise<UnreadFields> {
  let snapshot: UnreadFields | undefined;
  await expect
    .poll(
      async () => {
        const resp = await callApi(request, 'i', { i: token });
        if (resp.status() !== 200) return false;
        const body = (await resp.json()) as Partial<UnreadFields>;
        snapshot = {
          unreadNotificationsCount: body.unreadNotificationsCount ?? 0,
          hasUnreadNotification: body.hasUnreadNotification ?? false,
        };
        return predicate(snapshot);
      },
      { timeout: 5000, intervals: [100, 200, 500, 1000] },
    )
    .toBe(true);
  // expect.poll が toBe(true) を満たした iteration で必ず snapshot が set
  // される。!-assertion ではなく guard で表現することで、predicate match と
  // snapshot 設定の同期関係を読み手に明示する (pollForNotification と同 pattern)。
  if (!snapshot) {
    throw new Error('pollUnread: matched but `snapshot` was not set (unreachable)');
  }
  return snapshot;
}

test.describe('notifications: mark-all-as-read', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('mark-all-as-read clears unreadNotificationsCount and hasUnreadNotification', async ({
    request,
  }) => {
    const me = await signupUser(request, randomUsername('mraA'));
    const trigger = await signupUser(request, randomUsername('mraB'));

    const ws = await openStream(me.token);
    const subId = subscribeChannel(ws, 'main');

    // notification event 受信 = notification persistence の barrier として使う。
    const notifEventPromise = awaitChannelEvent<NotificationBody>(
      ws,
      (env) =>
        env.id === subId && env.type === 'notification' && env.body?.type === 'follow',
    );

    await awaitSubscribeBuffer();

    // trigger が me を follow → me に follow notification が積まれる。
    const followResp = await callApi(request, 'following/create', {
      i: trigger.token,
      userId: me.id,
    });
    expect(followResp.status()).toBeGreaterThanOrEqual(200);
    expect(followResp.status()).toBeLessThan(300);

    await notifEventPromise;
    ws.close();

    // /api/i で unread 系 field が増えたことを確認 (await persistence)。
    const before = await pollUnread(request, me.token, (u) => u.unreadNotificationsCount >= 1);
    expect(before.unreadNotificationsCount).toBeGreaterThanOrEqual(1);
    expect(before.hasUnreadNotification).toBe(true);

    // mark-all-as-read を呼ぶ (204 No Content)。
    const markResp = await callApi(request, 'notifications/mark-all-as-read', { i: me.token });
    expect(markResp.status()).toBeGreaterThanOrEqual(200);
    expect(markResp.status()).toBeLessThan(300);

    // /api/i で unread 系 field がゼロ化されたことを確認。
    // mark-all-as-read 自体は同期だが、TS の Redis 反映遅延を吸収して polling。
    const after = await pollUnread(request, me.token, (u) => u.unreadNotificationsCount === 0);
    expect(after.unreadNotificationsCount).toBe(0);
    expect(after.hasUnreadNotification).toBe(false);
  });
});
