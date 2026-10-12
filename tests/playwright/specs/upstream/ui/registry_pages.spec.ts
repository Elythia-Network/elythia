/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// レジストリの閲覧ページを開く (#2441)。
//
//   /registry                            スコープ一覧
//   /registry/keys/:domain/:path         キー一覧
//   /registry/value/:domain/:path        値の詳細
//
// レジストリはクライアント設定やプラグインの保存先で、ここが読めないと利用者は
// 自分のデータを確認できない。API (`i/registry/*`) は
// `specs/upstream/api/i/registry.spec.ts` が見ているが、画面側は未検証だった。
//
// domain の `@` は「システム (アプリ非依存)」を指す。URL 上も `@` で、
// これを取り違えるとキーが 1 つも出ない。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: registry', () => {
  let root: RootFixture;
  const scope = 'pwreg';
  const key = 'pwkey';

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('スコープ・キー・値の 3 画面が辿れる', async ({ page, baseURL, request }) => {
    const value = `pwvalue-${Date.now().toString().slice(-9)}`;
    const set = await callApi(request, 'i/registry/set', {
      i: root.token,
      scope: [scope],
      key,
      value,
    });
    expect(set.status()).toBeLessThan(400);

    await uiSigninAsRoot(page, baseURL, root);

    // 1. スコープ一覧。登録したスコープが出る。
    const top = await page.goto(`${baseURL}/registry`, { waitUntil: 'domcontentloaded' });
    expect(top!.status()).toBe(200);
    await expect(page.getByText(scope, { exact: false }).first()).toBeVisible({ timeout: 20_000 });

    // 2. キー一覧。domain は `@` (システム)。
    const keys = await page.goto(`${baseURL}/registry/keys/@/${scope}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(keys!.status()).toBe(200);
    await expect(page.getByText(key, { exact: false }).first()).toBeVisible({ timeout: 20_000 });

    // 3. 値の詳細。保存した値そのものが出る。
    const detail = await page.goto(`${baseURL}/registry/value/@/${scope}/${key}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(detail!.status()).toBe(200);
    await expect(page.getByText(value, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
  });
});
