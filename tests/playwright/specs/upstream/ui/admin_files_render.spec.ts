/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/files page で host filter / user ID / MIME type input + 行政
// 用 file list paginator が hydrate されることを smoke する spec。
//
// /admin/files は admin/drive/files paginator + MkSelect (origin) +
// MkInput x3 (host / userId / type) を mount する。filter UI が必ず存在する
// ので、input が 3 つ以上 visible になることを hydration sign にする。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/files page hydrates filter inputs', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('host / userId / MIME type inputs appear on /admin/files', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/files`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // host / userId / type filter は MkInput (= <input>) なので、
    // input 数 >= 3 で「filter UI が hydrate された」と判定。
    await page.waitForFunction(
      () => document.querySelectorAll('input').length >= 3,
      { timeout: 20_000 },
    );
  });
});
