/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/email-settings で送信元のメールアドレスを書き換えて保存すると、admin/meta
// の email が変わることを確かめる。
//
// 以前は値を変えずに Save を押し、応答が返ることだけを見ていた (#4025)。
// アドレスの欄はメールを有効にしているときだけ出るので、API で有効にしてから開く。
// 始める前の値を覚えておき、終わったら戻す (他の spec と meta を共有するため)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonByText } from '../../../fixtures/ui_click';
import { fillInputByLabel } from '../../../fixtures/ui_input';
import { coversRoutes } from '../../../fixtures/ui_route';

test.describe('UI: /admin/email-settings save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('edit Email address → Save → admin/meta has it', coversRoutes('/admin/email-settings'), async ({ page, baseURL, request }) => {
    const beforeResp = await callApi(request, 'admin/meta', { i: root.token });
    // 読めなかったまま進むと、元の値が undefined になり、片付けが何も戻さない
    expect(beforeResp.ok(), 'admin/meta を読めなかった').toBe(true);
    const before = await beforeResp.json();
    expect(before).toHaveProperty('enableEmail');
    const original = { enableEmail: before.enableEmail as boolean, email: before.email as string | null };
    const email = `pw-${Date.now()}@example.invalid`;

    try {
      await callApi(request, 'admin/update-meta', { i: root.token, enableEmail: true });
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/email-settings`, { waitUntil: 'domcontentloaded' });

      await fillInputByLabel(page, 'Email address', email);
      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/admin/update-meta') && r.status() < 300,
        { timeout: 15_000 },
      );
      await clickButtonByText(page, 'Save');
      await updateResp;

      const after = await (await callApi(request, 'admin/meta', { i: root.token })).json();
      expect(after.email).toBe(email);
    } finally {
      const restored = await callApi(request, 'admin/update-meta', { i: root.token, ...original });
      // 戻せなかったことは記録するが、元の失敗は隠さない (soft)
      expect.soft(restored.ok(), '元の値に戻せなかった').toBe(true);
    }
  });
});
