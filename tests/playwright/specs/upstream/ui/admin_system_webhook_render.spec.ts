/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/system-webhook page で admin/system-webhook/create で登録した
// webhook の name が render されることを verify する spec。
//
// /admin/system-webhook は admin/system-webhook/list を fetch して各
// webhook を XItem (system-webhook.item.vue) で render する。XItem は
// webhook.name を text として表示するので body 検索可。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/system-webhook renders configured webhooks', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('admin/system-webhook/create + /admin/system-webhook renders webhook name', async ({
    page,
    baseURL,
    request,
  }) => {
    const webhookName = `pwwh-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'admin/system-webhook/create', {
      i: root.token,
      isActive: true,
      name: webhookName,
      on: ['abuseReport'],
      url: 'https://playwright-webhook.invalid/hook',
      secret: '',
    });
    expect(createResp.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/system-webhook`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      webhookName,
      { timeout: 20_000 },
    );
  });
});
