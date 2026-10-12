/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #823 Phase 2 notification spec: renote notification の round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - user B が user A の note を renote (renoteId のみ、text 無しで投稿)
//     する → A の /api/i/notifications に `type: 'renote'` の notification
//     が登録される
//   - WS streaming main channel を subscribe していると同 notification
//     event が push される
//
// 注: renoteId + text が両方付くと quote 扱いになり notification type が
// 'quote' に変わるため、本 spec は pure renote (text 無し) で検証する。
//
// 本 spec は両 backend 共通で:
//   1. user A + user B signup
//   2. user A が public note 投稿
//   3. user A が main channel を WS subscribe
//   4. user B が note を renote (text 無し)
//   5. WS で notification event (type: 'renote') 受信
//   6. /api/i/notifications で同 notification を取得 (= persistent)
//
// reaction / mention / follow / reply spec と同 pattern (#823, #847)。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { type NotificationBody, pollForNotification } from '../../../../fixtures/notifications';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import {
  awaitChannelEvent,
  awaitSubscribeBuffer,
  openStream,
  subscribeChannel,
} from '../../../../fixtures/streaming';

test.describe('notifications: renote', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('renote of my note triggers notification', async ({ request }) => {
    const me = await signupUser(request, randomUsername('rntA'));
    const renoter = await signupUser(request, randomUsername('rntB'));

    const note = await createNote(request, me.token, {
      text: 'renote me',
      visibility: 'public',
    });

    const ws = await openStream(me.token);
    const subId = subscribeChannel(ws, 'main');

    const notifEventPromise = awaitChannelEvent<NotificationBody>(
      ws,
      (env) =>
        env.id === subId && env.type === 'notification' && env.body?.type === 'renote',
    );

    await awaitSubscribeBuffer();

    // renoter が note を renote (text 無しで pure renote、= quote ではない)。
    await createNote(request, renoter.token, {
      visibility: 'public',
      renoteId: note.id,
    });

    const evNotif = await notifEventPromise;
    expect(evNotif.type).toBe('renote');
    expect(evNotif.userId).toBe(renoter.id);

    ws.close();

    // /api/i/notifications でも同 notification が取得できる (= 永続化)。
    const httpNotif = await pollForNotification(
      request,
      me.token,
      (n) => n.type === 'renote' && n.userId === renoter.id,
    );
    expect(httpNotif.type).toBe('renote');
    expect(httpNotif.userId).toBe(renoter.id);
  });
});
