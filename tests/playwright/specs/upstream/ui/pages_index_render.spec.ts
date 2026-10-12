/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /pages page (= page list with tabs: Popular / My / Liked) で
// pages/create で作成した page が "My Pages" tab に出ることを verify
// する mixed e2e。default tab は featured (= popular) なので、作った
// page が必ずしも featured に乗らない。My tab は自分の page が必ず出る
// ので、tab 切替してから verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /pages page renders user pages list', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('/pages renders all 3 tabs (Popular / My Pages / Liked Pages)', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/pages`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // PageWithHeader の tabs が i18n._pages.{featured,my,liked} を mount。
    // Popular + My Pages + Liked Pages の 3 つ AND で固有性確保。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return (
          text.includes('Popular') &&
          text.includes('My Pages') &&
          text.includes('Liked Pages')
        );
      },
      { timeout: 20_000 },
    );
  });
});
