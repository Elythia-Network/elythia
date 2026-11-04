/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/object-storage で prefix を書き換えて保存すると、admin/meta の
// objectStoragePrefix が変わることを確かめる。
//
// 以前は値を変えずに Save を押し、応答が返ることだけを見ていた (#4025)。
// 欄はオブジェクトストレージを使うときだけ出るので、API で有効にしてから開く。
// 有効にしている間はドライブの保存先が変わるので、終わったら必ず元に戻す
// (同じスタックの spec は 1 本ずつ流れるので、その間に他の spec は動かない)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonByText } from '../../../fixtures/ui_click';
import { fillInputByLabel } from '../../../fixtures/ui_input';

test.describe('UI: /admin/object-storage save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('edit Prefix → Save → admin/meta has it', async ({ page, baseURL, request }) => {
    const beforeResp = await callApi(request, 'admin/meta', { i: root.token });
    // 読めなかったまま進むと、元の値が undefined になり、片付けが何も戻さない
    expect(beforeResp.ok(), 'admin/meta を読めなかった').toBe(true);
    const before = await beforeResp.json();
    expect(before).toHaveProperty('objectStoragePrefix');
    const originalPrefix = before.objectStoragePrefix as string | null;
    const prefix = `pw-${Date.now()}`;

    try {
      await callApi(request, 'admin/update-meta', { i: root.token, useObjectStorage: true });
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/object-storage`, { waitUntil: 'domcontentloaded' });

      await fillInputByLabel(page, 'Prefix', prefix);
      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/admin/update-meta') && r.status() < 300,
        { timeout: 15_000 },
      );
      await clickButtonByText(page, 'Save');
      await updateResp;

      const after = await (await callApi(request, 'admin/meta', { i: root.token })).json();
      expect(after.objectStoragePrefix).toBe(prefix);
    } finally {
      // オブジェクトストレージは、元の値によらず必ず無効に戻す。テストのスタックでは
      // 使わない前提で、一度 true が残ると、以後のドライブのアップロードの spec が
      // すべて落ちる (元の値に戻す形だと、残った true を戻し続ける)
      const restored = await callApi(request, 'admin/update-meta', { i: root.token, useObjectStorage: false, objectStoragePrefix: originalPrefix });
      // 戻せなかったことは記録するが、元の失敗は隠さない (soft)
      expect.soft(restored.ok(), '元の値に戻せなかった').toBe(true);
    }
  });
});
