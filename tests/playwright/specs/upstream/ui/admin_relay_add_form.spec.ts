/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/relays で header の "+ Add relay" button click → MkDialog
// (input dialog) → inbox URL 入力 → OK → /api/admin/relays/add round-trip
// する **真の write-flow** spec。
//
// admin/relays.vue の addRelay() は os.inputText() を使う。MkDialog の
// 構造は list_create_form.spec.ts と同じ (data-cy-modal-dialog-ok 経由
// の OK button + modal 内 input)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /admin/relays add form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('click "+ Add relay" → fill inbox URL → OK → /api/admin/relays/add', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/relays`, { waitUntil: 'domcontentloaded' });

    // header の "+" button (ti-plus) が hydrate するまで待つ
    await page.waitForFunction(
      () => document.querySelector('button i.ti-plus') !== null,
      { timeout: 20_000 },
    );

    // "+" header action click → os.inputText popup (= MkDialog)
    await clickButtonWithIcon(page, 'i.ti-plus');

    // MkDialog が open するまで待つ
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    const inboxURL = `https://relay-${Date.now().toString().slice(-9)}.example.test/inbox`;

    // dialog 内 last input (= MkDialog の MkInput) に書き込み
    await page.evaluate((u) => {
      const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
      const target = inputs[inputs.length - 1];
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, u);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, inboxURL);

    // admin/relays/add response 捕捉して OK click
    const addResp = page.waitForResponse(
      (r) => r.url().includes('/api/admin/relays/add') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    const resp = await addResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
