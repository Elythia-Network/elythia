/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/announcements page で admin/announcements/create + list が動作
// して MkFolder の label に announcement.title が出ることを verify する
// spec。/announcements (= 公開ページ) は content_pages_extra.spec.ts で
// 既に cover 済だが、本 spec は **admin 側の管理 page** の hydration を
// 確認する別 layer。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/announcements renders admin-side announcement list', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('admin/announcements/create + /admin/announcements renders the title', async ({
    page,
    baseURL,
    request,
  }) => {
    const title = `pwann-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'admin/announcements/create', {
      i: root.token,
      title,
      text: 'playwright admin announcement',
      imageUrl: null,
    });
    expect(createResp.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/announcements`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // 各 announcement は MkFolder の label slot に title を入れて
    // collapsed で render される。label の text は body.textContent に
    // 含まれる。
    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      title,
      { timeout: 20_000 },
    );
  });
});
