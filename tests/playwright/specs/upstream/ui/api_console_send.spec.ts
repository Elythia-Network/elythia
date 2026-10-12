/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /api-console で endpoint MkInput と params MkTextarea を埋めて Send
// button click → response area に JSON が render される **真の write-flow**
// spec。simple endpoint "i" を選んで自分の user info を取得する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /api-console send flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('fill endpoint "i" → click Send → response shows username', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/api-console`, { waitUntil: 'domcontentloaded' });

    // endpoint MkInput + body MkTextarea + Send button が hydrate
    await page.waitForFunction(
      () => {
        const inputs = document.querySelectorAll('input').length;
        const textareas = document.querySelectorAll('textarea').length;
        return inputs >= 1 && textareas >= 1;
      },
      { timeout: 20_000 },
    );

    // endpoint input (= 最初の input) に "i" を書き込み (= /api/i 呼び出し)。
    // body textarea は default '{}' のまま (= no params)。
    await page.evaluate(() => {
      const target = document.querySelector('input') as HTMLInputElement | null;
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, 'i');
      target.dispatchEvent(new Event('input', { bubbles: true }));
    });

    // /api/i response を捕捉して Send click
    const apiResp = page.waitForResponse(
      (r) => r.url().includes('/api/i') && !r.url().includes('/api/i/') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Send');
    await apiResp;

    // response textarea (= readonly textarea) に root.username を含む
    // JSON が表示される
    await page.waitForFunction(
      (n) => {
        const tas = Array.from(document.querySelectorAll('textarea')) as HTMLTextAreaElement[];
        return tas.some((ta) => ta.value.includes(n));
      },
      root.username,
      { timeout: 20_000 },
    );
  });
});
