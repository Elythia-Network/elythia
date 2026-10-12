/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/avatar-decorations page で admin/avatar-decorations/create で
// 作成した decoration の name が body に出るのを verify する spec。
//
// /admin/avatar-decorations は admin/avatar-decorations/list を fetch して
// 各 decoration を MkCondensedLine + MkAvatar で render する。decoration
// の name 文字列で hydration sign を取る。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/avatar-decorations renders configured decorations', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('admin/avatar-decorations/create + page renders the name', async ({
    page,
    baseURL,
    request,
  }) => {
    const decoName = `pwdeco-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'admin/avatar-decorations/create', {
      i: root.token,
      name: decoName,
      description: 'playwright test deco',
      url: 'https://example.invalid/playwright-deco.png',
      roleIdsThatCanBeUsedThisDecoration: [],
    });
    expect(createResp.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/avatar-decorations`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      decoName,
      { timeout: 20_000 },
    );
  });
});
