/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/profile の "advancedSettings" MkFolder を expand → isBot
// switch (folder 内 2 番目の checkbox) を toggle → /api/i/update が走る
// write-flow spec。
//
// profile.vue の advancedSettings 折り畳み内には isCat / isBot 2 個の
// MkSwitch があり、expand 後の checkbox 列の (folder 前 N 個) + 2 番目が
// isBot。サインアップ直後は folder 前は 0 個なので isBot = index 1。
// (profile.vue:142-147)

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/profile isBot toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('expand advancedSettings folder → toggle isBot → /api/i/update', async ({
    page,
    baseURL,
    request,
  }) => {
    // 値 strict assertion のため初期 state を false (= DB default) に reset。
    await callApi(request, 'i/update', { i: root.token, isBot: false });

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/profile`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      () => document.querySelectorAll('input').length >= 2,
      { timeout: 20_000 },
    );

    const beforeCheckboxes = await page.evaluate(
      () => document.querySelectorAll('input[type="checkbox"]').length,
    );

    // settings/profile の MkFolder は metadataEdit と advancedSettings の 2 つ。
    // 旧実装は `headers[1]` で advancedSettings を取っていたが、上部 settings
    // sidebar (= MkSuperMenu) や SearchMarker inlining で MkFolder が増減する
    // 可能性があるため、**i18n label "Advanced settings" で identify**。
    // en-US.yml の `advancedSettings: "Advanced settings"` を直 reference。
    await clickWhenReady(page, '「Advanced settings」の folder-header', () => {
      const headers = Array.from(
        document.querySelectorAll('[data-testid="folder-header"]'),
      ) as HTMLElement[];
      const target = headers.find((h) =>
        (h.textContent ?? '').includes('Advanced settings'),
      );
      return target;
    });
    await page.waitForFunction(
      (n) => document.querySelectorAll('input[type="checkbox"]').length >= n + 2,
      beforeCheckboxes,
      { timeout: 10_000 },
    );

    // isCat (index = beforeCheckboxes) の次 = isBot (index + 1)
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, 'isBot の checkbox', (before) => {
      const cbs = Array.from(
        document.querySelectorAll('input[type="checkbox"]'),
      ) as HTMLInputElement[];
      return cbs[before + 1];
    }, beforeCheckboxes);
    const update = await updateResp;
    const body = await update.json();
    // beforeAll の API reset で false から始まるので、click 後は必ず true
    // (isBot は UserDetailed = UserLite 由来 field なので i/update body に
    // 元から含まれる)。
    expect(body.id).toBeTruthy();
    expect(body.isBot).toBe(true);
  });
});
