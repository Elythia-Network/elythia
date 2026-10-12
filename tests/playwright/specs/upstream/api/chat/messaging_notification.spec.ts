/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #822 Phase 2 chat spec / #823 close: chat message の WS dedicated channel
// による notification + /api/i の hasUnreadChatMessages round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - chat の通知は /api/i/notifications には載らない (queue 経由ではなく
//     dedicated stream channel 経由で push する設計)
//   - WS の `chatUser` channel を `params: { otherId: <相手 user id> }` で
//     subscribe しておくと、相手 → 自分の DM が `type: 'message'` の event
//     として送られる。topic は内部で `chatUserStream:{me}-{other}`
//   - DM 受信側の /api/i では `hasUnreadChatMessages: true` にフラグが立つ
//   - chat/messages/user-timeline で会話を読み出すと既読化される副作用
//     (#692 readUserChatMessage 相当) があり、再度 /api/i を叩くと
//     `hasUnreadChatMessages` は false に戻る
//
// 本 spec は両 backend 共通で:
//   1. user A (sender) + user B (receiver) signup
//   2. receiver の chatScope を `everyone` に開放 (default `mutual` だと
//      無関係 user 間で reject されるため、本 spec の主眼ではない)
//   3. receiver が chatUser channel を subscribe (otherId: sender.id)
//   4. sender が receiver に chat/messages/create-to-user で DM 送信
//   5. receiver の WS で `type: 'message'` event を受信 (= chat notification
//      の dedicated path)、body の id / fromUserId / text が一致
//   6. /api/i で hasUnreadChatMessages = true (unread)
//   7. receiver が chat/messages/user-timeline を読み出して既読化
//   8. /api/i で hasUnreadChatMessages = false (= read 反映)
//
// これにより #823 (notification scope) の chatMessage 部分は dedicated
// channel + unread field の round-trip でカバー完了する。

import { expect, test, type APIRequestContext } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { type ChatMessage } from '../../../../fixtures/chat';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import {
  awaitChannelEvent,
  awaitSubscribeBuffer,
  openStream,
  subscribeChannel,
} from '../../../../fixtures/streaming';

// /api/i の hasUnreadChatMessages を expect.poll で predicate を満たすまで
// 待つ。chat の write は同期だが、TS は Redis 反映遅延を吸収するため
// polling する (notifications spec の pollUnread と同 pattern)。
async function pollHasUnreadChat(
  request: APIRequestContext,
  token: string,
  predicate: (v: boolean) => boolean,
): Promise<boolean> {
  let snapshot: boolean | undefined;
  await expect
    .poll(
      async () => {
        const resp = await callApi(request, 'i', { i: token });
        if (resp.status() !== 200) return false;
        const body = (await resp.json()) as { hasUnreadChatMessages?: boolean };
        snapshot = body.hasUnreadChatMessages ?? false;
        return predicate(snapshot);
      },
      { timeout: 5000, intervals: [100, 200, 500, 1000] },
    )
    .toBe(true);
  // expect.poll が toBe(true) を満たした iteration で必ず snapshot が set
  // される。!-assertion ではなく guard で predicate match と同期関係を明示。
  if (snapshot === undefined) {
    throw new Error('pollHasUnreadChat: matched but `snapshot` was not set (unreachable)');
  }
  return snapshot;
}

test.describe('chat: message notification (chatUser WS + hasUnreadChatMessages)', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('DM send pushes WS event and flips hasUnreadChatMessages, user-timeline read clears it', async ({
    request,
  }) => {
    const sender = await signupUser(request, randomUsername('cmsA'));
    const receiver = await signupUser(request, randomUsername('cmsB'));

    // receiver の chatScope を `everyone` に開放 (#822 PR-A と同根拠)。
    const scopeResp = await callApi(request, 'i/update', {
      i: receiver.token,
      chatScope: 'everyone',
    });
    expect(scopeResp.status()).toBeGreaterThanOrEqual(200);
    expect(scopeResp.status()).toBeLessThan(300);

    // receiver が chatUser channel を sender 相手で subscribe。
    const ws = await openStream(receiver.token);
    const subId = subscribeChannel(ws, 'chatUser', { otherId: sender.id });

    // 'message' event 受信用 Promise を投稿前に登録 (= race 回避)。
    const msgEventPromise = awaitChannelEvent<ChatMessage>(
      ws,
      (env) => env.id === subId && env.type === 'message',
    );

    await awaitSubscribeBuffer();

    // sender が receiver に DM 送信。
    const text = 'chat ws ' + Math.random().toString(16).slice(2, 8);
    const sendResp = await callApi(request, 'chat/messages/create-to-user', {
      i: sender.token,
      toUserId: receiver.id,
      text,
    });
    expect(sendResp.status()).toBe(200);
    const sent = (await sendResp.json()) as ChatMessage;

    // WS で 'message' event を受信。body 内の id / fromUser / text が一致する。
    const evMsg = await msgEventPromise;
    expect(evMsg.id).toBe(sent.id);
    expect(evMsg.fromUserId).toBe(sender.id);
    expect(evMsg.text).toBe(text);

    ws.close();

    // /api/i で hasUnreadChatMessages = true (= unread DM の存在を示す)。
    const unreadBefore = await pollHasUnreadChat(request, receiver.token, (v) => v === true);
    expect(unreadBefore).toBe(true);

    // user-timeline 読み出しで既読化 (mark-as-read 副作用)。
    const tlResp = await callApi(request, 'chat/messages/user-timeline', {
      i: receiver.token,
      userId: sender.id,
      limit: 10,
    });
    expect(tlResp.status()).toBe(200);

    // /api/i で hasUnreadChatMessages = false (= 既読化反映)。
    const unreadAfter = await pollHasUnreadChat(request, receiver.token, (v) => v === false);
    expect(unreadAfter).toBe(false);
  });
});
