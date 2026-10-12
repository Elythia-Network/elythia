/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /scratchpad page で MkCodeEditor + Run button + Output container が
// hydrate されることを smoke する spec。
//
// scratchpad は AiScript playground (= /play/:id とは別の sandbox)。
// MkCodeEditor は CodeMirror で AiScript モードを mount するので、
// hydration sign は code editor の textarea / "Output" container labels
// で十分。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /scratchpad page hydrates AiScript playground', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('scratchpad editor + output container appear on /scratchpad', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/scratchpad`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // hydration 完了 = MkCodeEditor (CodeMirror) の textarea / output
    // container header が render される。MkContainer の "UI inspector" /
    // "Output" のような label が body に出るのを sign にする。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        // i18n の Output / UI inspector 文字列。test 環境は 英語 default。
        return text.includes('Output') && text.includes('UI inspector');
      },
      { timeout: 20_000 },
    );
  });
});
