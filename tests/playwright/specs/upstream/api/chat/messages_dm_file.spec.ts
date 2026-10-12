/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #855 PR-C: chat DM with file attachment の round-trip。
//
// upstream Misskey TS の packedChatMessageSchema は `file: optional/nullable
// (DriveFile)` を持ち、ChatEntityService.packMessageDetailed は fileId が
// set されている message に対し file object を eager pack する。
//
// 本 spec は両 backend 共通で:
//   1. sender + receiver signup
//   2. receiver の chatScope を `everyone` に開放
//   3. sender が drive/files/create で tinyPNG upload (= fileId 確定)
//   4. sender が chat/messages/create-to-user で fileId 付き DM 送信
//   5. receiver の user-timeline で取得した message が `file` field を含み、
//      DriveFile shape (id / name / type / size) が upload と整合する
//
// upstream / mk-go ともに create-to-user response 自体に file を含める
// (= packMessageDetailed 経由)。mk-go は #860 PR-D で create response 後に
// File を post-fetch する形に整え、両 backend で同 shape を返すようにした。
// 本 spec は create response と user-timeline 両方で file shape を strict
// assert する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { type ChatMessage } from '../../../../fixtures/chat';
import { type DriveFile, tinyPNG } from '../../../../fixtures/files';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

const baseURL = process.env.MK_BASE_URL ?? 'https://mkgo.local';

interface ChatMessageWithFile extends ChatMessage {
  fileId?: string;
  file?: DriveFile;
}

test.describe('chat: DM with file attachment', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('A uploads a file then sends DM with fileId; B receives file shape via user-timeline', async ({
    request,
  }) => {
    const sender = await signupUser(request, randomUsername('dmfA'));
    const receiver = await signupUser(request, randomUsername('dmfB'));

    // receiver の chatScope を `everyone` に開放 (#822 PR-A と同根拠)。
    const scopeResp = await callApi(request, 'i/update', {
      i: receiver.token,
      chatScope: 'everyone',
    });
    expect(scopeResp.status()).toBeGreaterThanOrEqual(200);
    expect(scopeResp.status()).toBeLessThan(300);

    // sender が tinyPNG を drive/files/create で upload。
    const uploadResp = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: {
        i: sender.token,
        file: {
          name: 'chat-attach.png',
          mimeType: 'image/png',
          buffer: tinyPNG,
        },
      },
      failOnStatusCode: false,
    });
    expect(uploadResp.status()).toBe(200);
    const file = (await uploadResp.json()) as DriveFile;
    expect(typeof file.id).toBe('string');
    expect(file.name).toBe('chat-attach.png');
    expect(file.type).toBe('image/png');
    expect(file.size).toBe(tinyPNG.length);

    // sender が file 添付 DM 送信。
    const text = 'attach ' + Math.random().toString(16).slice(2, 8);
    const sendResp = await callApi(request, 'chat/messages/create-to-user', {
      i: sender.token,
      toUserId: receiver.id,
      text,
      fileId: file.id,
    });
    expect(sendResp.status()).toBe(200);
    const sent = (await sendResp.json()) as ChatMessageWithFile;
    expect(sent.fileId).toBe(file.id);
    // create response 直後でも file field が含まれることを strict assert
    // (#860 PR-D で post-fetch するように修正)。
    if (!sent.file) {
      throw new Error(`create response (id=${sent.id}) did not include file field despite fileId set`);
    }
    expect(sent.file.id).toBe(file.id);
    expect(sent.file.name).toBe('chat-attach.png');
    expect(sent.file.type).toBe('image/png');
    expect(sent.file.size).toBe(tinyPNG.length);

    // receiver の user-timeline で取得 → file field が DriveFile shape で
    // 含まれていることを確認 (Preload("File") 経路で eager load される)。
    const tlResp = await callApi(request, 'chat/messages/user-timeline', {
      i: receiver.token,
      userId: sender.id,
      limit: 10,
    });
    expect(tlResp.status()).toBe(200);
    const tl = (await tlResp.json()) as ChatMessageWithFile[];

    const found = tl.find((m) => m.id === sent.id);
    if (!found) {
      throw new Error(`user-timeline did not contain sent message (id=${sent.id})`);
    }
    expect(found.fileId).toBe(file.id);
    // file field が DriveFile shape を持つ。upstream / mk-go (#855 PR-C) で
    // 揃う minimal field (id / name / type / size) を strict assert。
    if (!found.file) {
      throw new Error(
        `user-timeline message (id=${sent.id}) did not include file field despite fileId set`,
      );
    }
    expect(found.file.id).toBe(file.id);
    expect(found.file.name).toBe('chat-attach.png');
    expect(found.file.type).toBe('image/png');
    expect(found.file.size).toBe(tinyPNG.length);
  });
});
