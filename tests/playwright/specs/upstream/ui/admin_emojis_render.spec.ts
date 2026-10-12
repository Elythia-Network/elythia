/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/emojis page で admin/emoji/add 経由で登録した custom emoji が
// 一覧 render されることを verify する spec。
//
// upstream Misskey は admin の emoji 一覧を MkPagination で表示し pageSize
// 単位で fetch するので、累積 test 実行で emoji 数が多くなると最新 emoji が
// 初回 page に乗らない。よって本 spec は 2 段で smoke する:
//   1. 新規 emoji 登録 → admin/emoji/list で API 上に存在することを直接 verify
//   2. /admin/emojis を navigate して MkPagination が hydrate (= 既存 emoji
//      button が 1 つ以上 mount される) ことを verify
// この組み合わせで「register API」「emoji 一覧 API → MkPagination 描画」の
// 両 path を carve out できる。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/emojis page renders admin-side emoji list', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('admin/emoji/add registers an emoji and /admin/emojis hydrates the list', async ({ page, baseURL, request }) => {
    // drive に dummy png を upload (emoji image source)
    const driveResp = await request.post(`${baseURL}/api/drive/files/create`, {
      ignoreHTTPSErrors: true,
      multipart: {
        i: root.token,
        file: {
          name: 'pw-emoji.png',
          mimeType: 'image/png',
          buffer: Buffer.from(
            'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
            'base64',
          ),
        },
      },
    });
    expect(driveResp.status()).toBe(200);
    const driveFile = await driveResp.json();
    expect(driveFile.id).toBeTruthy();

    // 一意 emoji name (アルファベット小文字 + 数字)。timestamp 後方 9 桁。
    const emojiName = `pwemo${Date.now().toString().slice(-9)}`;
    const addResp = await callApi(request, 'admin/emoji/add', {
      i: root.token,
      name: emojiName,
      fileId: driveFile.id,
      category: null,
      aliases: [],
      license: null,
      isSensitive: false,
      localOnly: false,
      roleIdsThatCanBeUsedThisEmojiAsReaction: [],
    });
    expect(addResp.status()).toBe(200);

    // API 上に新 emoji が存在すること (= admin/emoji/list で query して name match)
    const listResp = await callApi(request, 'admin/emoji/list', {
      i: root.token,
      query: emojiName,
      limit: 10,
    });
    expect(listResp.status()).toBe(200);
    const listed = await listResp.json();
    expect(Array.isArray(listed)).toBe(true);
    expect(listed.some((e: { name: string }) => e.name === emojiName)).toBe(true);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/emojis`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // MkPagination が hydrate して emoji button が 1 つ以上 mount される
    // (= 一覧 fetch の API 経路 + button 描画 chain が壊れていない)。
    // 各 emoji は `<button><img alt=name /><span>name</span></button>` で
    // mount されるので、img を child に持つ button が 1 つ以上あれば OK。
    // 個別 emoji の検出は API 側で済ませている。
    await page.waitForFunction(
      () => Array.from(document.querySelectorAll('button')).some((b) => b.querySelector('img') !== null),
      { timeout: 20_000 },
    );
  });
});
