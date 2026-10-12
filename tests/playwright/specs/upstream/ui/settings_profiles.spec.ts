/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/profiles (設定プロファイルの管理) をブラウザで開く (#2441)。
//
// 一度は「API から用意できない」として見送ったが、**出所はレジストリだった**。
// `listCloudBackups()` は `i/registry/keys` を scope
// `['client', 'preferences', 'backups']` で読むだけなので、`i/registry/set` で
// 事前に 1 件置けば一覧に出せる。
//
// 中身が無い状態だと `MkFolder` が 1 つも描画されず、assert できるのが左メニューの
// 文言だけになる (= `/settings/*` のどの URL でも通る偽陽性)。**必ずデータを
// 用意してから開く**のが要点。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /settings/profiles', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('保存済みプロファイルが一覧に表示される', async ({ page, baseURL, request }) => {
    const name = `pwprofile${Date.now().toString().slice(-9)}`;
    const set = await callApi(request, 'i/registry/set', {
      i: root.token,
      scope: ['client', 'preferences', 'backups'],
      key: name,
      // 実際のバックアップは設定一式だが、一覧はキー名だけを見る。
      value: { name, createdAt: '2026-01-01T00:00:00.000Z' },
    });
    expect(set.status()).toBeLessThan(400);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/settings/profiles`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // 保存したプロファイルが出ないと、利用者は復元も削除もできない。
    await expect(page.getByText(name, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
  });
});
