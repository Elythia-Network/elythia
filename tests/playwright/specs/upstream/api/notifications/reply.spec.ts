/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #823 Phase 2 notification spec: reply notification の round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - user B が user A の note に reply (replyId 付き note 投稿) する →
//     A の /api/i/notifications に `type: 'reply'` の notification が
//     登録される
//   - WS streaming main channel を subscribe していると同 notification
//     event が push される
//
// 本 spec は両 backend 共通で:
//   1. user A + user B signup
//   2. user A が public note 投稿
//   3. user A が main channel を WS subscribe
//   4. user B が note に reply
//   5. WS で notification event (type: 'reply') 受信
//   6. /api/i/notifications で同 notification を取得 (= persistent)
//
// reaction / mention / follow spec と同 pattern (#823, #847)。

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

test.describe('notifications: reply', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('reply to my note triggers notification', async ({ request }) => {
    const me = await signupUser(request, randomUsername('rplA'));
    const replier = await signupUser(request, randomUsername('rplB'));

    // me が public note 投稿。subscribe より前で良い (note 自体は notification
    // event の trigger ではないため race の対象外)。
    const note = await createNote(request, me.token, {
      text: 'reply to me',
      visibility: 'public',
    });

    const ws = await openStream(me.token);
    const subId = subscribeChannel(ws, 'main');

    const notifEventPromise = awaitChannelEvent<NotificationBody>(
      ws,
      (env) =>
        env.id === subId && env.type === 'notification' && env.body?.type === 'reply',
    );

    await awaitSubscribeBuffer();

    // replier が note に reply。
    await createNote(request, replier.token, {
      text: 'replying',
      visibility: 'public',
      replyId: note.id,
    });

    const evNotif = await notifEventPromise;
    expect(evNotif.type).toBe('reply');
    expect(evNotif.userId).toBe(replier.id);

    ws.close();

    // /api/i/notifications でも同 notification が取得できる (= 永続化)。
    const httpNotif = await pollForNotification(
      request,
      me.token,
      (n) => n.type === 'reply' && n.userId === replier.id,
    );
    expect(httpNotif.type).toBe('reply');
    expect(httpNotif.userId).toBe(replier.id);
  });
});
