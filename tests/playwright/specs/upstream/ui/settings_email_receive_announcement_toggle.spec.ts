/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/email で receiveAnnouncementEmail switch (1st) を click →
// /api/i/update が round-trip する write-flow spec。
//
// email.vue:26 の MkSwitch :modelValue="$i.receiveAnnouncementEmail" は
// 直接 i/update を叩く onChangeReceiveAnnouncementEmail を呼ぶ
// (manualSave じゃない、即時保存)。switch 順は receiveAnnouncementEmail /
// emailNotification_* (5個) の計 6 で、本 spec は 1 番目を toggle する。
//
// #973 (PR fix #972) で i/update が receiveAnnouncementEmail を accept する
// ように修正されたので、value 反映を strict assertion で verify できる。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/email receiveAnnouncementEmail toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('toggle receiveAnnouncementEmail switch (1st) → /api/i/update round-trips', async ({
    page,
    baseURL,
    request,
  }) => {
    // 値 strict assertion のため初期 state を true (= DB default) に reset。
    await callApi(request, 'i/update', { i: root.token, receiveAnnouncementEmail: true });

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/email`, { waitUntil: 'domcontentloaded' });

    // 6 個以上の checkbox が hydrate するまで待つ
    await page.waitForFunction(
      () => document.querySelectorAll('input[type="checkbox"]').length >= 6,
      { timeout: 20_000 },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '1 番目の checkbox', () => {
      const cbs = Array.from(
        document.querySelectorAll('input[type="checkbox"]'),
      ) as HTMLInputElement[];
      // index 0 = 1 番目 = receiveAnnouncementEmail
      return cbs[0];
    });
    const update = await updateResp;
    const body = await update.json();
    expect(body.id).toBeTruthy();
    // beforeAll で true → click 後は必ず false (#973 で i/update accept、
    // #969 PackMeDetailed で response 含む)。
    expect(body.receiveAnnouncementEmail).toBe(false);
  });
});
