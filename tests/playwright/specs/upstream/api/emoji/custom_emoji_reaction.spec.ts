/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #824 PR-A emoji spec: admin/emoji/add で custom emoji を登録し、
// その emoji を note に reaction として付与する round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - admin/emoji/add (admin only) で custom emoji を登録、200 + 登録 emoji
//     を return
//   - 登録した emoji は notes/reactions/create で `:name:` (host 省略) で
//     付与でき、サーバーは `:name@.:` (canonical local form) に normalize
//   - notes/show で取得した note.reactions object の key も canonical form
//     `:name@.:`、value は付与数
//
// upstream は admin/emoji/add で `fileId` (drive 経由) を必須としている。
// mk-go は legacy で `url` 直接受けも維持しつつ、本 PR で `fileId` 受け付け
// path を追加して両 backend で同じ flow が動く。
//
// 本 spec は両 backend 共通で:
//   1. globalSetup が用意した root (admin) credentials を読み込む
//   2. root が drive/files/create で tinyPNG を upload (= fileId 取得)
//   3. admin が `fileId` 経由で custom emoji を登録
//   4. reactor user signup
//   5. root が public note 投稿
//   6. reactor が note に `:<name>:` reaction → サーバー normalize
//   7. notes/show で reactions に `:<name>@.:` が 1 件反映
//
// これで #821 残 scope (カスタム絵文字 reaction の id 解決と shape 確認) も
// 派生でカバーされる。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { tinyPNG } from '../../../../fixtures/files';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

const baseURL = process.env.MK_BASE_URL ?? 'https://mkgo.local';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

test.describe('emoji: custom emoji add + reaction round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('admin adds custom emoji and reactor uses :name: which normalizes to :name@.:', async ({
    request,
  }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));

    // global で衝突しないよう random suffix 付き name を生成。
    const emojiName = 'spec_emoji_' + Math.random().toString(16).slice(2, 8);

    // upstream Misskey TS は admin/emoji/add で fileId を必須とするため、
    // 先に drive/files/create で image を upload して fileId を取得する。
    // mk-go も #824 PR-A で fileId 受け付けに対応 (= 本 spec と同 PR で
    // handler 拡張)。
    const uploadResp = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: {
        i: root.token,
        file: {
          name: emojiName + '.png',
          mimeType: 'image/png',
          buffer: tinyPNG,
        },
      },
      failOnStatusCode: false,
    });
    expect(uploadResp.status()).toBe(200);
    const uploaded = (await uploadResp.json()) as { id: string };

    // admin が fileId 経由で custom emoji を追加。
    const addResp = await callApi(request, 'admin/emoji/add', {
      i: root.token,
      name: emojiName,
      fileId: uploaded.id,
    });
    expect(addResp.status()).toBe(200);
    const emoji = (await addResp.json()) as { id: string };
    expect(typeof emoji.id).toBe('string');
    expect(emoji.id.length).toBeGreaterThan(0);

    const reactor = await signupUser(request, randomUsername('emR'));

    // root が public note 投稿。
    const note = await createNote(request, root.token, {
      text: 'react with custom',
      visibility: 'public',
    });

    // reactor が `:name:` (host 省略) で reaction 付与 → サーバーで
    // `:name@.:` に normalize される。
    const reactResp = await callApi(request, 'notes/reactions/create', {
      i: reactor.token,
      noteId: note.id,
      reaction: ':' + emojiName + ':',
    });
    expect(reactResp.status()).toBeGreaterThanOrEqual(200);
    expect(reactResp.status()).toBeLessThan(300);

    // notes/show で取得した reactions object に canonical form
    // `:<name>@.:` が 1 件反映されている。
    const showResp = await callApi(request, 'notes/show', {
      i: root.token,
      noteId: note.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as { reactions: Record<string, number> };
    const canonicalKey = ':' + emojiName + '@.:';
    expect(Object.keys(shown.reactions)).toEqual([canonicalKey]);
    expect(shown.reactions[canonicalKey]).toBe(1);
  });
});
