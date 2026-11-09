/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// チャットの独自の API (CherryPick 由来の口と、Elythia で足した口) を叩き、応答を
// /api.json の宣言で照合する (#4093)。本家にもある口 (rooms/create・invitations/create
// など) は前提を作るためだけに callApi で叩く。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { callChecked } from '../../../fixtures/api_schema';
import { type Principal, randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { type ChatMessage, type ChatRoom } from '../../../fixtures/chat';
import { resetRateLimit } from '../../../fixtures/rate_limit';

type Request = Parameters<typeof callApi>[0];

/** Calls an upstream endpoint that only sets up the state, and returns its body. */
async function setup<T = unknown>(request: Request, endpoint: string, body: Record<string, unknown>): Promise<T> {
  const resp = await callApi(request, endpoint, body);
  expect(resp.status(), `${endpoint}: ${await resp.text()}`).toBeLessThan(300);
  return (resp.status() === 204 ? undefined : await resp.json()) as T;
}

async function invite(request: Request, owner: Principal, roomId: string, user: Principal): Promise<void> {
  await setup(request, 'chat/rooms/invitations/create', { i: owner.token, roomId, userId: user.id });
}

test.describe('chat: the Elythia endpoints answer as declared', () => {
  test.skip(isTsBackend, 'ここで叩く口は Elythia 独自');

  test.beforeAll(() => {
    resetRateLimit();
  });

  test('a direct message: send, count, list, read, edit and react', async ({ request }) => {
    const a = await signupUser(request, randomUsername('chda'));
    const b = await signupUser(request, randomUsername('chdb'));
    // 受け手の chatScope の既定は相互フォローなので、誰からでも受けるようにする
    await setup(request, 'i/update', { i: b.token, chatScope: 'everyone' });

    const sent = await callChecked<ChatMessage>(request, 'chat/messages/create', { i: a.token, toUserId: b.id, text: 'hello' });
    expect(sent.toUserId).toBe(b.id);
    // 一覧は未読の数より先に取る。一覧を取るだけで既読になる形に変わっても、read の確かめが空振りしない
    const listed = await callChecked<ChatMessage[]>(request, 'chat/messages', { i: b.token, userId: a.id });
    expect(listed.map((m) => m.id)).toContain(sent.id);
    expect((await callChecked<{ count: number }>(request, 'chat/unread-count', { i: b.token })).count).toBe(1);

    await callChecked(request, 'chat/messages/read', { i: b.token, messageId: sent.id });
    expect((await callChecked<{ count: number }>(request, 'chat/unread-count', { i: b.token })).count).toBe(0);

    await callChecked(request, 'chat/messages/update', { i: a.token, messageId: sent.id, text: 'edited' });
    await callChecked(request, 'chat/messages/reactions/create', { i: b.token, messageId: sent.id, reaction: '👍' });
    const reacted = await setup<{ text: string; reactions: { reaction: string }[] }>(request, 'chat/messages/show', { i: a.token, messageId: sent.id });
    expect(reacted.text).toBe('edited');
    expect(reacted.reactions.map((r) => r.reaction)).toEqual(['👍']);
    await callChecked(request, 'chat/messages/reactions/delete', { i: b.token, messageId: sent.id, reaction: '👍' });
    expect((await setup<{ reactions: unknown[] }>(request, 'chat/messages/show', { i: a.token, messageId: sent.id })).reactions).toEqual([]);
  });

  test('a room: invitations, messages, members and the owner', async ({ request }) => {
    const owner = await signupUser(request, randomUsername('chro'));
    const member = await signupUser(request, randomUsername('chrm'));
    const other = await signupUser(request, randomUsername('chrx'));
    const room = await setup<ChatRoom>(request, 'chat/rooms/create', { i: owner.token, name: 'room', description: '' });

    // 承諾・断る・取り消すの 3 通り
    await invite(request, owner, room.id, member);
    await callChecked(request, 'chat/rooms/invitations/accept', { i: member.token, roomId: room.id });
    await invite(request, owner, room.id, other);
    await callChecked(request, 'chat/rooms/invitations/reject', { i: other.token, roomId: room.id });
    expect(await setup<unknown[]>(request, 'chat/rooms/invitations/inbox', { i: other.token })).toEqual([]);
    await invite(request, owner, room.id, other);
    const outbox = await setup<{ id: string; userId: string }[]>(request, 'chat/rooms/invitations/outbox', { i: owner.token, roomId: room.id });
    const pending = outbox.find((inv) => inv.userId === other.id);
    expect(pending, 'the invitation to other is pending').toBeDefined();
    await callChecked(request, 'chat/rooms/invitations/delete', { i: owner.token, invitationId: pending!.id });
    expect(await setup<unknown[]>(request, 'chat/rooms/invitations/inbox', { i: other.token })).toEqual([]);

    const joined = await callChecked<ChatRoom[]>(request, 'chat/rooms/joined', { i: member.token });
    expect(joined.map((r) => r.id)).toEqual([room.id]);
    const sent = await callChecked<ChatMessage>(request, 'chat/messages/create', { i: owner.token, toRoomId: room.id, text: 'hi all' });
    expect(sent.toRoomId).toBe(room.id);
    const listed = await callChecked<ChatMessage[]>(request, 'chat/messages', { i: member.token, roomId: room.id });
    expect(listed.map((m) => m.id)).toContain(sent.id);

    // owner がメンバーをミュートし、メンバーが自分で解く
    await callChecked(request, 'chat/rooms/members/update-membership', { i: owner.token, roomId: room.id, userId: member.id, isMuted: true });
    expect((await setup<ChatRoom>(request, 'chat/rooms/show', { i: member.token, roomId: room.id })).isMuted).toBe(true);
    await callChecked(request, 'chat/rooms/unmute', { i: member.token, roomId: room.id });
    expect((await setup<ChatRoom>(request, 'chat/rooms/show', { i: member.token, roomId: room.id })).isMuted).toBe(false);

    // 譲渡すると前の owner はメンバーになり、新しい owner が締め出せる
    await callChecked(request, 'chat/rooms/transfer-ownership', { i: owner.token, roomId: room.id, userId: member.id });
    expect((await setup<ChatRoom>(request, 'chat/rooms/show', { i: member.token, roomId: room.id })).ownerId).toBe(member.id);
    expect((await callChecked<ChatRoom[]>(request, 'chat/rooms/joined', { i: owner.token })).map((r) => r.id)).toEqual([room.id]);
    await callChecked(request, 'chat/rooms/members/ban', { i: member.token, roomId: room.id, userId: owner.id });
    expect(await callChecked<ChatRoom[]>(request, 'chat/rooms/joined', { i: owner.token })).toEqual([]);
  });
});
