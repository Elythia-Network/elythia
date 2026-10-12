/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /share?text=... page で MkPostForm が prefill 済の状態で hydrate される
// ことを smoke する spec。
//
// /share は外部 web からの共有 entry point (= Misskey hub 仕様)。
// initialText query param が MkPostForm の textarea に bind されるので、
// data-cy-post-form-text の value に prefill されることを verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /share page hydrates with prefilled text', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('/share?text=<msg> prefills MkPostForm textarea', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);

    const message = `pwshare-${Date.now().toString().slice(-9)}`;
    const resp = await page.goto(`${baseURL}/share?text=${encodeURIComponent(message)}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // MkPostForm は data-cy-post-form-text の textarea を mount し、
    // initialText (URL query 由来) が value に bind される。
    await page.waitForFunction(
      (m) => {
        const t = document.querySelector(
          '[data-testid="post-form-text"]',
        ) as HTMLTextAreaElement | null;
        return t !== null && t.value.includes(m);
      },
      message,
      { timeout: 20_000 },
    );
  });
});
