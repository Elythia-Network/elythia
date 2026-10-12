/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/avatar-decorations で header "+ Add" → MkWindow popup → name +
// imageUrl 入力 → "Create" button click → /api/admin/avatar-decorations/create
// round-trip する **真の write-flow** spec。
//
// avatar-decorations.vue の add() は popupAsyncWithDialog で
// avatar-decoration-edit-dialog.vue を開く (MkWindow ベース)。dialog 内に
// name MkInput + imageUrl MkInput + description MkTextarea が並ぶ。Create
// click は MkButton primary で "Create" textContent (= avatarDecoration が
// null のとき)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: /admin/avatar-decorations create form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('click + → fill name+imageUrl → Create → admin/avatar-decorations/create round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/avatar-decorations`, {
      waitUntil: 'domcontentloaded',
    });

    // header の "+" button (ti-plus) を待つ
    await page.waitForFunction(
      () => document.querySelector('button i.ti-plus') !== null,
      { timeout: 20_000 },
    );

    const baselineInputs = await page.evaluate(
      () => document.querySelectorAll('input').length,
    );

    // "+" header click → edit dialog popup
    await clickButtonWithIcon(page, 'i.ti-plus');

    // dialog 内 input が +2 (name / imageUrl) されたら expand 完了
    await page.waitForFunction(
      (n) => document.querySelectorAll('input').length >= n + 2,
      baselineInputs,
      { timeout: 10_000 },
    );

    const decoName = `pwdecoui-${Date.now().toString().slice(-9)}`;
    const decoUrl = 'https://example.test/decoration.png';

    // dialog 内 input 2 個 (name = 1 つ目、imageUrl = 2 つ目) を埋める。
    // page baseline 以降の input が dialog のもの。
    await page.evaluate(
      ({ n, u, base }) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        const dialogInputs = inputs.slice(base);
        const setter = Object.getOwnPropertyDescriptor(
          window.HTMLInputElement.prototype,
          'value',
        )?.set;
        const setValue = (el: HTMLInputElement | undefined, v: string) => {
          if (!el) return;
          el.focus();
          setter?.call(el, v);
          el.dispatchEvent(new Event('input', { bubbles: true }));
        };
        setValue(dialogInputs[0], n);
        setValue(dialogInputs[1], u);
      },
      { n: decoName, u: decoUrl, base: baselineInputs },
    );

    // admin/avatar-decorations/create response 捕捉して "Create" click
    const createResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/admin/avatar-decorations/create') && r.status() < 400,
      { timeout: 15_000 },
    );
    // dialog 内の "Create" button (= MkButton primary、textContent に
    // "Create" を含む)
    await clickButtonByText(page, 'Create');
    const resp = await createResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
