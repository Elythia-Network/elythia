/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /api-console page で endpoint MkInput + datalist が hydrate される
// ことを smoke する spec。
//
// API console は misskeyApi 経由で各 endpoint を直接叩ける開発者 page。
// MkInput (endpoint 名) + textarea (parameters) + Run button が render
// される。endpoint datalist は misskey-js から取得される hardcode list。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /api-console page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('API console page renders endpoint input + send button', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/api-console`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // page title "API console" は <title> 経由で textContent には出ないが、
    // 本体の "Endpoint" label (i18n.ts.endpoint → "Endpoint") + Send button
    // が render される。MkInput 1 + textarea 1 + button 1 が最低条件。
    await page.waitForFunction(
      () => {
        const inputs = document.querySelectorAll('input').length;
        const textareas = document.querySelectorAll('textarea').length;
        const buttons = document.querySelectorAll('button').length;
        return inputs >= 1 && textareas >= 1 && buttons >= 1;
      },
      { timeout: 20_000 },
    );
  });
});
