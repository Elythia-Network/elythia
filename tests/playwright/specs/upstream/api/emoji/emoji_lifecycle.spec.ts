/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #824 残: admin/emoji/{add, update, list, delete} の lifecycle
// round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - admin/emoji/add { name, fileId } で custom emoji 追加 (200 + entity)
//   - admin/emoji/update { id, name?, category?, aliases?, ... } で
//     name / category 等を更新 (204 No Content)
//   - admin/emoji/list { query?, category? } で filter 一覧
//   - admin/emoji/delete { id } で削除 (204)
//
// 本 spec は両 backend 共通で 1 emoji の lifecycle を round-trip:
//   1. globalSetup root を読み込み
//   2. drive/files/create で tinyPNG upload (= fileId 取得)
//   3. admin/emoji/add → 登録 emoji の id を取得
//   4. admin/emoji/list で uniq name 検索 → 含まれること
//   5. admin/emoji/update で category 設定 → 反映 (再度 list で category 一致)
//   6. admin/emoji/delete → 一覧から消える
//
// PR #867 (#824 PR-A) は add + reaction の round-trip にフォーカスしていた
// ので、本 spec は残 lifecycle (update / list / delete) を補完する scope。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { tinyPNG } from '../../../../fixtures/files';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

const baseURL = process.env.MK_BASE_URL ?? 'https://mkgo.local';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

interface AdminEmojiListEntry {
  id: string;
  name: string;
  category?: string | null;
}

test.describe('emoji: admin lifecycle (add / update / list / delete)', () => {
  // assertion 失敗時に DB に orphan emoji が残らないよう、test 単位で
  // afterEach cleanup する。describe scope に id を hoist して closure で
  // capture する pattern (= /api/emojis spec の try/finally と同等の安全性)。
  let createdEmojiId: string | undefined;
  let rootToken: string | undefined;

  test.beforeAll(() => {
    resetRateLimit();
  });

  test.afterEach(async ({ request }) => {
    if (createdEmojiId && rootToken) {
      await callApi(request, 'admin/emoji/delete', {
        i: rootToken,
        id: createdEmojiId,
      });
    }
    createdEmojiId = undefined;
    rootToken = undefined;
  });

  test('admin adds, updates, lists, then deletes a custom emoji', async ({
    request,
  }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    rootToken = root.token;
    const emojiName = 'spec_lc_' + Math.random().toString(16).slice(2, 8);

    // upload tinyPNG → fileId 取得
    const uploadResp = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: {
        i: root.token,
        file: { name: emojiName + '.png', mimeType: 'image/png', buffer: tinyPNG },
      },
      failOnStatusCode: false,
    });
    expect(uploadResp.status()).toBe(200);
    const uploaded = (await uploadResp.json()) as { id: string };

    // add
    const addResp = await callApi(request, 'admin/emoji/add', {
      i: root.token,
      name: emojiName,
      fileId: uploaded.id,
    });
    expect(addResp.status()).toBe(200);
    const added = (await addResp.json()) as { id: string };
    expect(typeof added.id).toBe('string');
    createdEmojiId = added.id;

    // add 直後 list で query 検索 → 追加 emoji が含まれること
    const listAfterAdd = await callApi(request, 'admin/emoji/list', {
      i: root.token,
      query: emojiName,
      limit: 10,
    });
    expect(listAfterAdd.status()).toBe(200);
    const listAfterAddBody = (await listAfterAdd.json()) as AdminEmojiListEntry[];
    const foundAfterAdd = listAfterAddBody.find((e) => e.id === added.id);
    expect(foundAfterAdd).toBeDefined();
    expect(foundAfterAdd!.name).toBe(emojiName);

    // update で category を設定
    const newCategory = 'spec-cat-' + Math.random().toString(16).slice(2, 6);
    const updResp = await callApi(request, 'admin/emoji/update', {
      i: root.token,
      id: added.id,
      category: newCategory,
    });
    expect(updResp.status()).toBe(204);

    // update 後 list で category が反映されていること
    const listAfterUpdate = await callApi(request, 'admin/emoji/list', {
      i: root.token,
      query: emojiName,
      limit: 10,
    });
    expect(listAfterUpdate.status()).toBe(200);
    const listAfterUpdateBody = (await listAfterUpdate.json()) as AdminEmojiListEntry[];
    const foundAfterUpdate = listAfterUpdateBody.find((e) => e.id === added.id);
    expect(foundAfterUpdate).toBeDefined();
    expect(foundAfterUpdate!.category).toBe(newCategory);

    // delete
    const delResp = await callApi(request, 'admin/emoji/delete', {
      i: root.token,
      id: added.id,
    });
    expect(delResp.status()).toBe(204);
    // 正規 path で delete 済 = afterEach cleanup の必要なし。
    createdEmojiId = undefined;

    // delete 後 list から消えていること
    const listAfterDelete = await callApi(request, 'admin/emoji/list', {
      i: root.token,
      query: emojiName,
      limit: 10,
    });
    expect(listAfterDelete.status()).toBe(200);
    const listAfterDeleteBody = (await listAfterDelete.json()) as AdminEmojiListEntry[];
    expect(listAfterDeleteBody.some((e) => e.id === added.id)).toBe(false);
  });
});
