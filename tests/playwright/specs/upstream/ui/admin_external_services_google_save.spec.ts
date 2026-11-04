/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/external-services の「Google Analytics」のフォルダで Measurement ID を
// 書き換えて保存すると、admin/meta の googleAnalyticsMeasurementId が変わることを
// 確かめる。
//
// 以前は値を変えずに Save を押し、応答が返ることだけを見ていた (#4025)。
// Save はフォルダごとにあるので、フォルダの中のものを押す。
// 始める前の値を覚えておき、終わったら戻す (他の spec と meta を共有するため)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickFolderButton, openFolderByLabel } from '../../../fixtures/ui_folder';
import { fillInputByLabel } from '../../../fixtures/ui_input';

const FOLDER = 'Google Analytics';

test.describe('UI: /admin/external-services Google Analytics save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('edit Measurement ID → Save → admin/meta has it', async ({ page, baseURL, request }) => {
    const beforeResp = await callApi(request, 'admin/meta', { i: root.token });
    // 読めなかったまま進むと、元の値が undefined になり、片付けが何も戻さない
    expect(beforeResp.ok(), 'admin/meta を読めなかった').toBe(true);
    const before = await beforeResp.json();
    expect(before).toHaveProperty('googleAnalyticsMeasurementId');
    const original: string | null = before.googleAnalyticsMeasurementId;
    const id = `G-PW${Date.now().toString().slice(-8)}`;

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/external-services`, { waitUntil: 'domcontentloaded' });

      await openFolderByLabel(page, FOLDER);
      await fillInputByLabel(page, 'Measurement ID', id);
      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/admin/update-meta') && r.status() < 300,
        { timeout: 15_000 },
      );
      await clickFolderButton(page, FOLDER, 'Save');
      await updateResp;

      const after = await (await callApi(request, 'admin/meta', { i: root.token })).json();
      expect(after.googleAnalyticsMeasurementId).toBe(id);
    } finally {
      const restored = await callApi(request, 'admin/update-meta', { i: root.token, googleAnalyticsMeasurementId: original });
      // 戻せなかったことは記録するが、元の失敗は隠さない (soft)
      expect.soft(restored.ok(), '元の値に戻せなかった').toBe(true);
    }
  });
});
