/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #822 Phase 2 chat spec: DM (user-to-user message) の round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - chat/messages/create-to-user で sender が receiver に DM を送ると
//     ChatMessage object (id / fromUserId / toUserId / text / createdAt 等)
//     が 200 OK で返る
//   - receiver は chat/messages/user-timeline { userId: <sender> } で
//     対話相手の DM 一覧を取得できる
//   - user-timeline 側は initial load で「相手→自分」の DM をすべて
//     既読マークする副作用を持つ (#692, upstream の readUserChatMessage 相当)
//
// 本 spec は両 backend 共通で:
//   1. user A + user B signup
//   2. A → B に chat/messages/create-to-user で text 付き DM 送信
//   3. response shape (id / fromUserId / toUserId / text / createdAt) を assert
//   4. B 側で chat/messages/user-timeline { userId: A.id } 取得
//   5. timeline list 内に送信した message が含まれることを確認
//
// 注: room / membership / chatMessage notification は別 PR で扱う (scope 分割)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { type ChatMessage } from '../../../../fixtures/chat';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('chat: DM messages', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('A sends DM to B; B can read it back via user-timeline', async ({ request }) => {
    const sender = await signupUser(request, randomUsername('dmA'));
    const receiver = await signupUser(request, randomUsername('dmB'));

    // receiver の chatScope は default が "mutual" (相互フォロー必須) のため、
    // signup 直後の 2 user 間では DM が reject される (#692)。spec の意図は
    // chatScope policy ではなく DM round-trip 自体なので、receiver 側で
    // "everyone" に開放してから送る。chatScope 別の policy は専用 spec で扱う。
    const scopeResp = await callApi(request, 'i/update', {
      i: receiver.token,
      chatScope: 'everyone',
    });
    expect(scopeResp.status()).toBeGreaterThanOrEqual(200);
    expect(scopeResp.status()).toBeLessThan(300);

    const text = 'hello DM ' + Math.random().toString(16).slice(2, 8);

    // sender が receiver に DM 送信。
    const sendResp = await callApi(request, 'chat/messages/create-to-user', {
      i: sender.token,
      toUserId: receiver.id,
      text,
    });
    expect(sendResp.status()).toBe(200);
    const sent = (await sendResp.json()) as ChatMessage;

    // response shape strict assert (id / fromUser / toUser / text / createdAt)。
    // upstream / mk-go で揃っていなければ drop-in 互換 regression。
    expect(typeof sent.id).toBe('string');
    expect(sent.id.length).toBeGreaterThan(0);
    expect(sent.fromUserId).toBe(sender.id);
    expect(sent.toUserId).toBe(receiver.id);
    // upstream / mk-go ともに値が無い field を JSON response から omit する
    // (#851 fix 後)。本 spec の DM 投稿では room ではないため `toRoomId` は
    // undefined になる。
    expect(sent.toRoomId).toBeUndefined();
    expect(sent.text).toBe(text);
    // createdAt は ISO 8601 (`YYYY-MM-DDTHH:MM:SS.sssZ`)。Date.parse で
    // 有効な timestamp として読めることだけ確認 (秒精度の drift は許容)。
    expect(Number.isFinite(Date.parse(sent.createdAt))).toBe(true);

    // receiver 側で user-timeline 取得 → 対象 DM が含まれる。
    const tlResp = await callApi(request, 'chat/messages/user-timeline', {
      i: receiver.token,
      userId: sender.id,
      limit: 10,
    });
    expect(tlResp.status()).toBe(200);
    const tl = (await tlResp.json()) as ChatMessage[];
    expect(Array.isArray(tl)).toBe(true);

    // `find` の return が undefined なら spec の前提 (= sent message が
    // receiver の user-timeline で見える) が崩れているので明示 throw で
    // fail させる。!-assertion ではなく guard で型を narrow する pattern は
    // pollForNotification (#848) / pollUnread (#850) と揃える。
    const found = tl.find((m) => m.id === sent.id);
    if (!found) {
      throw new Error(`user-timeline did not contain sent message (id=${sent.id})`);
    }
    expect(found.fromUserId).toBe(sender.id);
    expect(found.toUserId).toBe(receiver.id);
    expect(found.text).toBe(text);
  });
});
