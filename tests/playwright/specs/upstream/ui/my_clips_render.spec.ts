/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/clips page で clips/create で作成した clip の name が MkClipPreview
// で render されることを verify する spec。
//
// /my/clips は clips/list (= 自分の clip) と clips/my-favorites の 2 tab
// 構成。default tab は 'my' で、MkPagination + MkClipPreview の chain で
// render する。content_pages.spec.ts で /clips/:id 詳細 page は cover 済
// だが、本 spec は **list page** の hydration を verify する別 layer。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /my/clips renders the user clip list', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('clips/create + /my/clips renders clip name', async ({ page, baseURL, request }) => {
    const clipName = `pwclip-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'clips/create', {
      i: root.token,
      name: clipName,
      isPublic: true,
    });
    expect(createResp.status()).toBe(200);
    const clip = await createResp.json();
    expect(clip.id, 'clips/create should return id').toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/clips`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // MkClipPreview は clip.name を text として render するので body 検索可
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      clipName,
      { timeout: 20_000 },
    );
  });
});
