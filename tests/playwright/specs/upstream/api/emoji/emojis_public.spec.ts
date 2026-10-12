/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #824 残: /api/emojis (public meta endpoint) round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - /api/emojis (auth 不要) で local custom emoji 一覧を返す
//   - response shape は { emojis: [{ name, category, aliases, url, ... }] }
//   - admin/emoji/add 後すぐ反映される (cache 経由でも upstream は最大数 sec)
//
// 本 spec は両 backend 共通で:
//   1. globalSetup root が tinyPNG upload + admin/emoji/add で uniq emoji を登録
//   2. /api/emojis を 認証なしで叩く
//   3. response.emojis 配列に追加 emoji が含まれること (= name match で hit)
//   4. cleanup として admin/emoji/delete で削除
//
// /api/emojis は frontend が起動時に一括 fetch する list なので drop-in
// 互換 (= shape / cache 含む) を担保する重要な path。

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

interface PublicEmoji {
  name: string;
  category?: string | null;
  url?: string | null;
}

test.describe('emoji: /api/emojis public list', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('newly added admin emoji appears in /api/emojis without auth', async ({
    request,
  }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    const emojiName = 'spec_pub_' + Math.random().toString(16).slice(2, 8);

    // upload + admin add
    const uploadResp = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: {
        i: root.token,
        file: { name: emojiName + '.png', mimeType: 'image/png', buffer: tinyPNG },
      },
      failOnStatusCode: false,
    });
    expect(uploadResp.status()).toBe(200);
    const uploaded = (await uploadResp.json()) as { id: string };

    const addResp = await callApi(request, 'admin/emoji/add', {
      i: root.token,
      name: emojiName,
      fileId: uploaded.id,
    });
    expect(addResp.status()).toBe(200);
    const added = (await addResp.json()) as { id: string };

    try {
      // /api/emojis は auth 不要 (= frontend 起動時 fetch)。upstream / mk-go
      // 共に cache を介する path だが、admin/emoji/add の応答後は反映済み
      // ことが期待される。万一 cache lag があっても本 spec は固定 wait を
      // 入れず、即時 fetch で hit を要求する (両 backend で実測 pass)。
      const listResp = await callApi(request, 'emojis', {});
      expect(listResp.status()).toBe(200);
      const body = (await listResp.json()) as { emojis: PublicEmoji[] };
      expect(Array.isArray(body.emojis)).toBe(true);
      const hit = body.emojis.find((e) => e.name === emojiName);
      expect(hit).toBeDefined();
      // url は public CDN / drive URL の form。空文字でなければ OK
      // (具体的な URL shape は backend / config 依存なので strict 比較しない)。
      expect(typeof hit!.url).toBe('string');
      expect((hit!.url || '').length).toBeGreaterThan(0);
    } finally {
      // 後続 spec の noise を防ぐため必ず cleanup する。
      await callApi(request, 'admin/emoji/delete', {
        i: root.token,
        id: added.id,
      });
    }
  });
});
