/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /@username の 3-dot menu (ti-dots) → menu の "Unmute" item (ti-fw ti-eye)
// を click → /api/mute/delete が直接 round-trip する write-flow spec。
//
// user/home.vue:37 の menu button (ti-dots) は getUserMenu (#820 で UI
// カバー) を popup する。menu items には mute / renote-mute / block の
// 3 toggle があり、user.isMuted=true のときは Unmute item (ti-eye) +
// 直接 API、isMuted=false のときは select dialog 経由。本 spec は前者を
// test するため、setup で root が target を mute 状態にしておく (= 直接
// API path を確認、popup menu 経由の最短 flow)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: user 3-dot menu unmute flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('signup target → root mutes via API → /@target menu → Unmute → /api/mute/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. target user を signup
    const target = await signupUser(request, `pwum${Date.now().toString().slice(-9)}`);
    expect(target.id).toBeTruthy();

    // 2. root が target を mute via API (= isMuted=true 状態を作る)
    const muteResp = await callApi(request, 'mute/create', {
      i: root.token,
      userId: target.id,
      expiresAt: null,
    });
    expect(muteResp.status()).toBeLessThan(400);

    // 3. /@target を root として開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/@${target.username}`, {
      waitUntil: 'domcontentloaded',
    });

    // username が body に出るまで待つ (= profile hydrate)
    await page.waitForFunction(
      (u) => document.body.textContent?.includes(u) ?? false,
      target.username,
      { timeout: 20_000 },
    );

    // 4. 3-dot menu button (= ti-dots) を click
    await clickButtonWithIcon(page, 'i.ti-dots');

    // 5. popup menu の "Unmute" item (= ti-fw ti-eye) を待って click
    // user.isMuted=true 時の icon は ti-eye、isMuted=false なら ti-eye-off。
    // menu icon は ti-fw 修飾を持つ (MkMenu.vue:56)。
    const unmuteResp = page.waitForResponse(
      (r) => r.url().includes('/api/mute/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickButtonWithIcon(page, 'i.ti-fw.ti-eye');
    await unmuteResp;

    // 6. API 経由で関係性 verify: root の muting list から target が消える
    const relResp = await callApi(request, 'users/relation', {
      i: root.token,
      userId: target.id,
    });
    expect(relResp.status()).toBe(200);
    // upstream users/relation は単一 userId (string) でも
    // `.then(it => [it])` で **必ず配列** を返す (relation.ts:135-137)。
    // mk-go も #1766 で配列に揃えているので、単体オブジェクトとして
    // 扱うと全 field が undefined になる。
    const [rel] = await relResp.json();
    expect(rel.isMuted).toBe(false);
  });
});
