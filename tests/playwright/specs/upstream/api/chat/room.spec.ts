/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #822 Phase 2 chat spec: room CRUD + invite + room messages の round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - chat/rooms/create で owner が room を作成 → ChatRoom object
//     (id / name / ownerId / description / isArchived) が 200 OK で返る
//   - chat/rooms/invitations/create { roomId, userId } で owner が invitee に
//     招待を送る (204)
//   - chat/rooms/invitations/inbox で invitee 側に届いた invitation list を
//     取得できる
//   - chat/rooms/join { roomId } で invite 受信側が room に参加する (204)。
//     upstream は invitations/accept endpoint を持たず、本 path で join する
//     設計のため両 backend 共通の参加 path として使う
//   - membership がある user は chat/messages/create-to-room { toRoomId, text }
//     で room 宛 message を投稿でき、chat/messages/room-timeline { roomId }
//     で room 内 message を取得できる
//
// 本 spec は両 backend 共通で:
//   1. owner + invitee signup
//   2. owner が room 作成 → response shape を assert
//   3. owner が invitee に invitation 作成
//   4. invitee が invitations/inbox で invitation 受信を確認
//   5. invitee が rooms/join で room 参加 (= membership 確立)
//   6. invitee が room に message 投稿 → response shape を assert
//   7. owner が room-timeline 取得 → 投稿 message が含まれることを確認
//
// 注: chatMessage notification は別 PR (PR-C) で扱う。本 spec は room CRUD +
// membership + room messages の最小 round-trip にフォーカス。
//
// shape: upstream の packedChatRoomSchema に揃え、`isArchived` は含まれない。
// chat message も値が無い field (toUserId 等) は JSON response から omit
// される (#851 fix 済)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { type ChatMessage, type ChatRoom } from '../../../../fixtures/chat';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('chat: rooms', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('owner creates room, invites user, user accepts and posts a message', async ({
    request,
  }) => {
    const owner = await signupUser(request, randomUsername('rmO'));
    const invitee = await signupUser(request, randomUsername('rmI'));

    // owner が room を作成。
    const roomName = 'rm-' + Math.random().toString(16).slice(2, 8);
    const createResp = await callApi(request, 'chat/rooms/create', {
      i: owner.token,
      name: roomName,
      description: 'spec room',
    });
    expect(createResp.status()).toBe(200);
    const room = (await createResp.json()) as ChatRoom;
    expect(typeof room.id).toBe('string');
    expect(room.id.length).toBeGreaterThan(0);
    expect(room.name).toBe(roomName);
    expect(room.ownerId).toBe(owner.id);
    expect(room.description).toBe('spec room');
    // createdAt は ISO 8601 (`YYYY-MM-DDTHH:MM:SS.sssZ`)。upstream は ID から
    // 派生する必須 field、mk-go は #855 PR-A 以降で同 pattern。Date.parse で
    // 有効な timestamp として読めることを check (秒精度の drift は許容)。
    expect(Number.isFinite(Date.parse(room.createdAt))).toBe(true);
    // owner === me で room を作成した直後なので、upstream / mk-go (#855 PR-B
    // 以降) ともに isMuted / invitationExists は false。owner check で repo
    // lookup が skip される設計。
    expect(room.isMuted).toBe(false);
    expect(room.invitationExists).toBe(false);
    // upstream の packedChatRoomSchema には `isArchived` が無いため
    // mk-go も #851 fix 以降は返さない。本 spec では archived state は
    // assert しない (= field 不在を許容)。

    // owner が invitee に invitation を作成 (204 No Content)。
    const inviteResp = await callApi(request, 'chat/rooms/invitations/create', {
      i: owner.token,
      roomId: room.id,
      userId: invitee.id,
    });
    expect(inviteResp.status()).toBeGreaterThanOrEqual(200);
    expect(inviteResp.status()).toBeLessThan(300);

    // invitee が invitations/inbox で受信した invitation を確認 (= invite が
    // 永続化されていることを assert)。invitations/inbox の round-trip 自体も
    // ここでカバーされる。
    const inboxResp = await callApi(request, 'chat/rooms/invitations/inbox', {
      i: invitee.token,
    });
    expect(inboxResp.status()).toBe(200);
    const inbox = (await inboxResp.json()) as {
      id: string;
      roomId: string;
      room?: ChatRoom;
    }[];
    const inv = inbox.find((x) => x.roomId === room.id);
    if (!inv) {
      throw new Error(`invitations/inbox did not contain invitation for room ${room.id}`);
    }
    // inbox の `room` field で invitee 視点の packRoomDetailed shape を確認:
    // me !== owner なので membership / invitation lookup が走り、
    // invitationExists = true (= 招待が pending)、isMuted = false (まだ
    // member ではないので mute 状態なし) になる (#855 PR-B)。
    // 両 backend で `room` field が必ず付く前提なので、不在は contract 違反
    // として明示 throw で fail させる (pollForNotification / tl.find と同
    // guard pattern)。
    if (!inv.room) {
      throw new Error(
        `invitations/inbox entry for room ${room.id} did not include room field`,
      );
    }
    expect(inv.room.id).toBe(room.id);
    expect(inv.room.invitationExists).toBe(true);
    expect(inv.room.isMuted).toBe(false);

    // invitee が rooms/join { roomId } で room に join (= membership 確立)。
    // upstream Misskey TS は invitations/accept endpoint を持たず、invite
    // 受信側は rooms/join を呼んで参加する設計。mk-go も rooms/join 単独で
    // membership を確立できる (invite チェック無しで join 可能なゆるさあり、
    // upstream の invite 必須は別 spec で扱う)。
    const joinResp = await callApi(request, 'chat/rooms/join', {
      i: invitee.token,
      roomId: room.id,
    });
    expect(joinResp.status()).toBeGreaterThanOrEqual(200);
    expect(joinResp.status()).toBeLessThan(300);

    // invitee が room に message 投稿 (200)。
    const text = 'room hello ' + Math.random().toString(16).slice(2, 8);
    const sendResp = await callApi(request, 'chat/messages/create-to-room', {
      i: invitee.token,
      toRoomId: room.id,
      text,
    });
    expect(sendResp.status()).toBe(200);
    const sent = (await sendResp.json()) as ChatMessage;
    expect(typeof sent.id).toBe('string');
    expect(sent.fromUserId).toBe(invitee.id);
    expect(sent.toRoomId).toBe(room.id);
    // upstream / mk-go ともに値が無い field を JSON response から omit する
    // (#851 fix 後)。本 spec の room message では DM ではないため `toUserId`
    // は undefined になる。
    expect(sent.toUserId).toBeUndefined();
    expect(sent.text).toBe(text);
    expect(Number.isFinite(Date.parse(sent.createdAt))).toBe(true);

    // owner が room-timeline で確認 → 投稿 message が含まれる。
    const tlResp = await callApi(request, 'chat/messages/room-timeline', {
      i: owner.token,
      roomId: room.id,
      limit: 10,
    });
    expect(tlResp.status()).toBe(200);
    const tl = (await tlResp.json()) as ChatMessage[];
    expect(Array.isArray(tl)).toBe(true);

    // `find` の return が undefined なら spec の前提 (= 投稿 message が
    // room-timeline で見える) が崩れているので明示 throw で fail させる。
    // pollForNotification (#848) / pollUnread (#850) / messages_dm (#852)
    // と同 guard pattern。
    const found = tl.find((m) => m.id === sent.id);
    if (!found) {
      throw new Error(`room-timeline did not contain sent message (id=${sent.id})`);
    }
    expect(found.fromUserId).toBe(invitee.id);
    expect(found.toRoomId).toBe(room.id);
    expect(found.text).toBe(text);
  });
});
