/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/database page で admin/get-table-stats 経由 DB table 一覧が
// MkKeyValue として render されることを verify する spec。
//
// 全 backend で table "drive_file" は必ず存在するので、"drive_file" key
// + value 末尾の "recs)" 文字列の組合せで hydration sign を取る ("user"
// だと "users" 等他箇所にも match して偽陽性になりうる)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/database page renders table stats', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('admin/get-table-stats hydrates table list on /admin/database', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/database`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // 各 table は MkKeyValue で `{name}` key + `{size} ({count} recs)`
    // value として render される。"recs)" は MkKeyValue value 末尾の
    // hardcode で他 page では使われない。加えて mk-go / Misskey TS 両方で
    // 必ず存在する table 名 "drive_file" を組み合わせて固有性を確保
    // ("user" だと "users" 等他箇所にも match)。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('drive_file') && text.includes('recs)');
      },
      { timeout: 20_000 },
    );
  });
});
