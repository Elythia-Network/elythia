/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /share?text=... に navigate → MkPostForm に prefill された note を
// submit button click で投稿 → API で note 取得確認する **真の write-flow**
// spec。share_render.spec.ts は prefill 値の verify までだったが、本 spec
// は実際に submit して backend に届くところまで cover する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /share submit creates note via form', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('/share?text=<msg> → click submit → /api/notes/create round-trips', async ({
    page,
    baseURL,
    request,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);

    const noteText = `pwshare-submit-${Date.now().toString().slice(-9)}`;
    const resp = await page.goto(`${baseURL}/share?text=${encodeURIComponent(noteText)}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // textarea が prefill されるまで待つ (= MkPostForm hydration 完了)。
    await page.waitForFunction(
      (t) => {
        const ta = document.querySelector(
          '[data-testid="post-form-text"]',
        ) as HTMLTextAreaElement | null;
        return ta !== null && ta.value.includes(t);
      },
      noteText,
      { timeout: 20_000 },
    );

    // submit button が disabled でなくなるまで待つ (= canPost === true)
    await page.waitForFunction(
      () => {
        const btn = document.querySelector(
          '[data-testid="post-form-submit"]',
        ) as HTMLButtonElement | null;
        return btn !== null && !btn.disabled;
      },
      { timeout: 5_000 },
    );

    // notes/create response を捕捉して click submit。post_note.spec.ts と
    // 同じ programmatic click pattern (= modal animation / hover state の
    // 干渉を避ける)
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/notes/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'post-form-submit');
    const created = await createResp;
    const body = await created.json();
    expect(body.createdNote.text).toBe(noteText);

    // backend で note を取得して text round-trip を verify
    const showResp = await callApi(request, 'notes/show', {
      i: root.token,
      noteId: body.createdNote.id,
    });
    expect(showResp.status()).toBe(200);
    expect((await showResp.json()).text).toBe(noteText);
  });
});
