/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #823 Phase 2 notification spec: receiveFollowRequest notification の round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - locked user (`isLocked: true`) に follow request が来る → followee 側の
//     /api/i/notifications に `type: 'receiveFollowRequest'` の notification
//     が登録される (通常 follow と異なり approval 待ち状態)
//   - WS streaming main channel を subscribe していると同 notification
//     event が push される
//
// 本 spec は両 backend 共通で:
//   1. user A signup → i/update で isLocked: true (follow approval 必須に)
//   2. user B signup
//   3. user A が main channel を WS subscribe
//   4. user B が user A を follow (= follow request 状態で pending)
//   5. WS で notification event (type: 'receiveFollowRequest') 受信
//   6. /api/i/notifications で同 notification を取得 (= persistent)
//
// follow.spec.ts は public account を対象とした immediate follow を扱う。
// 本 spec は locked account への follow request という別 path を担保する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { type NotificationBody, pollForNotification } from '../../../../fixtures/notifications';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import {
  awaitChannelEvent,
  awaitSubscribeBuffer,
  openStream,
  subscribeChannel,
} from '../../../../fixtures/streaming';

test.describe('notifications: receiveFollowRequest', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('follow request to locked account triggers notification', async ({ request }) => {
    const me = await signupUser(request, randomUsername('frqA'));

    // me を locked account にして follow approval 必須にする。
    // status は 200 / 204 が両 backend で揃って 2xx であることだけ保証する
    // (200 vs 204 drift の検出は i/update 専用 spec の責務)。
    const lockResp = await callApi(request, 'i/update', {
      i: me.token,
      isLocked: true,
    });
    expect(lockResp.status()).toBeGreaterThanOrEqual(200);
    expect(lockResp.status()).toBeLessThan(300);

    const requester = await signupUser(request, randomUsername('frqB'));

    const ws = await openStream(me.token);
    const subId = subscribeChannel(ws, 'main');

    const notifEventPromise = awaitChannelEvent<NotificationBody>(
      ws,
      (env) =>
        env.id === subId &&
        env.type === 'notification' &&
        env.body?.type === 'receiveFollowRequest',
    );

    await awaitSubscribeBuffer();

    // requester が me に follow request (locked なので approval pending)。
    const followResp = await callApi(request, 'following/create', {
      i: requester.token,
      userId: me.id,
    });
    expect(followResp.status()).toBeGreaterThanOrEqual(200);
    expect(followResp.status()).toBeLessThan(300);

    const evNotif = await notifEventPromise;
    expect(evNotif.type).toBe('receiveFollowRequest');
    expect(evNotif.userId).toBe(requester.id);

    ws.close();

    // /api/i/notifications でも同 notification が取得できる (= 永続化)。
    const httpNotif = await pollForNotification(
      request,
      me.token,
      (n) => n.type === 'receiveFollowRequest' && n.userId === requester.id,
    );
    expect(httpNotif.type).toBe('receiveFollowRequest');
    expect(httpNotif.userId).toBe(requester.id);
  });
});
