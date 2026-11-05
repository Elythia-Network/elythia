/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/privacy の autoAcceptFollowed のスイッチを押すと、/api/i/update で保存され、
// 読み直しても変わっていることを確かめる。
//
// スイッチはラベルの文字で取る (#4025)。以前は「n 番目の checkbox」で取っていて、
// 項目が 1 つ増えると別のスイッチを押していた。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickSwitchByLabel } from '../../../fixtures/ui_click';
import { coversRoutes } from '../../../fixtures/ui_route';

const LABEL = "Automatically approve follow requests from users you're following";

test.describe('UI: /settings/privacy autoAcceptFollowed toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('toggle the autoAcceptFollowed switch → saved and read back', coversRoutes('/settings/privacy'), async ({ page, baseURL, request }) => {
    // 押した後の値を決め打ちで確かめるため、既知の値 (false) から始める
    await callApi(request, 'i/update', { i: root.token, autoAcceptFollowed: false });

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/settings/privacy`, { waitUntil: 'domcontentloaded' });

      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/i/update') && r.status() === 200,
        { timeout: 15_000 },
      );
      await clickSwitchByLabel(page, LABEL);
      const body = await (await updateResp).json();
      // i/update は MeDetailed の形を返す (#968)
      expect(body.username).toBe(root.username);
      expect(body.autoAcceptFollowed).toBe(true);

      // 応答だけでなく、保存された値を読み直す
      const me = await (await callApi(request, 'i', { i: root.token })).json();
      expect(me.autoAcceptFollowed).toBe(true);
    } finally {
      // root は他の spec と共有なので、押す前の値に戻す
      const restored = await callApi(request, 'i/update', { i: root.token, autoAcceptFollowed: false });
      expect.soft(restored.ok(), '元の値に戻せなかった').toBe(true);
    }
  });
});
