/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1 残: streaming WebSocket の local timeline channel。
//
// upstream Misskey TS と mk-go は `/streaming?i=<token>` で WebSocket を提供
// し、クライアントが `connect` で channel を subscribe したあと該当 event を
// push する。本 spec は localTimeline channel で:
//
//   1. signup user → WS connect → localTimeline subscribe
//   2. 同 user で public note を投稿 (HTTP POST /api/notes/create)
//   3. WS message として `{type:"channel", body:{id:<subId>, type:"note", body:<note>}}`
//      が届く
//   4. event の note.id が POST 結果の note.id と一致
//
// を assert する。streaming は Phase 1 残 spec の中で WebSocket 経路を担保
// する重要 path。channel の payload 形式 / id round-trip / event push 経路
// が両 backend で揃うことを担保する。
//
// 注: subscribe → publish の race を避けるため subscribe 後に短時間 sleep
// を入れている。upstream は subscribe ack を返さないので message 受信の
// timeout 内に event が届くかで成否を判定する。
//
// Phase 1 残のもう一つ (2FA-passkey) は別 spec。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import {
  awaitChannelEvent,
  awaitSubscribeBuffer,
  openStream,
  subscribeChannel,
} from '../../../../fixtures/streaming';

test.describe('streaming: localTimeline', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('subscribed user receives a note event for a local public note', async ({ request }) => {
    const me = await signupUser(request, randomUsername('strm'));
    const ws = await openStream(me.token);
    const subId = subscribeChannel(ws, 'localTimeline');

    // 投稿 event 受信用の Promise を先に登録 (= 投稿 → 受信の race を防ぐ)。
    const noteEventPromise = awaitChannelEvent<{ id: string; text?: string }>(
      ws,
      (env) => env.id === subId && env.type === 'note',
    );

    // subscribe 確定までの短時間バッファ。upstream は ack を返さないので
    // 厳密に待つ手段はなく、200ms で実用上十分。
    await awaitSubscribeBuffer();

    const note = await createNote(request, me.token, {
      text: 'streaming hello',
      visibility: 'public',
    });

    const evNote = await noteEventPromise;
    expect(evNote.id).toBe(note.id);

    ws.close();
  });
});
