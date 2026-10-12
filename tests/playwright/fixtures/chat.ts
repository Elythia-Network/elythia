/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// chat spec 共通の型 (#822, #823)。messages_dm / room /
// messaging_notification の各 spec で chat/messages/* / chat/rooms/*
// response の最小 shape を共有する。

// ChatMessage は chat/messages/* response の最小 shape。upstream の
// `packedChatMessageSchema` (json-schema/chat-message.ts) と整合する形で、
// `toUserId` / `toRoomId` / `text` / `fileId` は optional として定義する。
// upstream / mk-go ともに値が無い場合は JSON response から field を omit する
// (#851 fix 後)。
export interface ChatMessage {
  id: string;
  fromUserId: string;
  toUserId?: string;
  toRoomId?: string;
  text?: string;
  createdAt: string;
}

// ChatRoom は chat/rooms/* response の最小 shape。upstream の
// `packedChatRoomSchema` に揃え、`isArchived` は含めない (#851 fix 後)。
// archived state は別経路で取得する設計。
//
// `createdAt` は upstream 必須 field (ID から派生)、mk-go も #855 PR-A
// 以降で全 chat/rooms/* endpoint が含めて返す。
//
// `isMuted` / `invitationExists` は upstream optional だが、mk-go は
// handler 経由 response で常に boolean を返す (#855 PR-B 以降)。
// owner === me の場合は両方 false (= owner は自身の room なので mute も
// invitation も無関係)、それ以外は membership / invitation lookup の結果。
export interface ChatRoom {
  id: string;
  createdAt: string;
  name: string;
  ownerId: string;
  description: string;
  isMuted: boolean;
  invitationExists: boolean;
}
