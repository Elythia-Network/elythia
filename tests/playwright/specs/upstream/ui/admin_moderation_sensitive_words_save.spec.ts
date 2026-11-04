/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/moderation の「Sensitive words」のフォルダを開き、textarea を書き換えて保存すると、
// admin/meta の sensitiveWords が変わることを確かめる。
//
// textarea と「Save」は、フォルダの見出しで決めたフォルダの中から取る (#4025)。
// 以前は画面の「1 つ目の textarea」を取り、保存の応答を待つだけで値を見ていなかった。
// 始める前の値を覚えておき、終わったら戻す (他の spec と meta を共有するため)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickFolderButton, fillFolderTextarea, openFolderByLabel } from '../../../fixtures/ui_folder';

const FOLDER = 'Sensitive words';

test.describe('UI: /admin/moderation sensitiveWords save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('edit the Sensitive words folder → Save → admin/meta has the new list', async ({ page, baseURL, request }) => {
    const beforeResp = await callApi(request, 'admin/meta', { i: root.token });
    // 読めなかったまま進むと、元の値が undefined になり、片付けが何も戻さない
    expect(beforeResp.ok(), 'admin/meta を読めなかった').toBe(true);
    const before = await beforeResp.json();
    const original: string[] = before.sensitiveWords;
    expect(Array.isArray(original)).toBe(true);

    const stamp = Date.now().toString().slice(-9);
    const lines = [`pwsens-${stamp}`, 'adult', 'nsfw'];

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/moderation`, { waitUntil: 'domcontentloaded' });

      await openFolderByLabel(page, FOLDER, { timeout: 30_000 });
      await fillFolderTextarea(page, FOLDER, lines.join('\n'));

      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/admin/update-meta') && r.status() < 300,
        { timeout: 15_000 },
      );
      await clickFolderButton(page, FOLDER, 'Save');
      await updateResp;

      // 応答を待つだけでなく、保存された値を読み直す
      const after = await (await callApi(request, 'admin/meta', { i: root.token })).json();
      expect(after.sensitiveWords).toEqual(lines);
    } finally {
      const restored = await callApi(request, 'admin/update-meta', { i: root.token, sensitiveWords: original });
      // 戻せなかったことは記録するが、元の失敗は隠さない (soft)
      expect.soft(restored.ok(), '元の値に戻せなかった').toBe(true);
    }
  });
});
