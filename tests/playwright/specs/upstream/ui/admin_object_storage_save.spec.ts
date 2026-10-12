/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/object-storage で "Save" button click → /api/admin/update-meta
// round-trip する **真の write-flow** spec。
//
// admin/object-storage.vue は S3-compat 設定 form。Save click で現在値を
// commit する (object-storage.vue:101 / line 137 で apiWithDialog)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /admin/object-storage save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('click Save → /api/admin/update-meta round-trips', async ({ page, baseURL, request }) => {
    // setup: object storage が誤って enable のまま残っていると drive
    // upload 系 spec が S3 接続失敗で全滅するので明示 false に戻す。
    await callApi(request, 'admin/update-meta', {
      i: root.token,
      useObjectStorage: false,
    });

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/object-storage`, { waitUntil: 'domcontentloaded' });

      await page.waitForFunction(
        () => {
          const btns = Array.from(document.querySelectorAll('button'));
          return btns.some((b) => (b.textContent ?? '').includes('Save'));
        },
        { timeout: 20_000 },
      );

      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/admin/update-meta') && r.status() < 400,
        { timeout: 15_000 },
      );
      await clickButtonContainingText(page, 'Save');
      const resp = await updateResp;
      expect(resp.status()).toBeLessThan(400);
    } finally {
      // cleanup: 必ず useObjectStorage=false に戻す。残ると以降の drive
      // upload spec が S3 (未設定) 接続で失敗する。
      await callApi(request, 'admin/update-meta', {
        i: root.token,
        useObjectStorage: false,
      });
    }
  });
});
