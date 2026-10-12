/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/clips で "+ Add" button click → MkFormDialog (name + description +
// isPublic の 3 field form) → name 入力 → "Done" click → /api/clips/create
// round-trip → 一覧に新 clip が並ぶ **真の write-flow** spec。
//
// my-clips/index.vue の create() は os.form() を使い MkFormDialog を popup
// する。MkFormDialog は MkModalWindow の `:withOkButton="true"` で本体下に
// "Done" button を載せる。fields は MkForm.vue で type='string' (=
// MkInput type=text) と type='boolean' (= MkSwitch) を render する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /my/clips create form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('click + Add → fill name → Done → /api/clips/create round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/my/clips`, { waitUntil: 'domcontentloaded' });

    // header "+ Add" button が hydrate するまで待つ (i18n.ts.add → "Add")
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button'));
        return btns.some((b) => (b.textContent ?? '').includes('Add'));
      },
      { timeout: 20_000 },
    );

    // dialog popup 前の input 数を baseline として記録 → click → dialog 内
    // input が +1 (名前 field) されたことで判定する。
    const baselineInputs = await page.evaluate(
      () => document.querySelectorAll('input').length,
    );

    // "+ Add" click → MkFormDialog popup
    await clickButtonContainingText(page, 'Add');

    // MkFormDialog の name MkInput (= form の text 入力 1 個) が出るまで待つ。
    await page.waitForFunction(
      (n) => document.querySelectorAll('input').length > n,
      baselineInputs,
      { timeout: 10_000 },
    );

    const clipName = `pwclipui-${Date.now().toString().slice(-9)}`;

    // form 内の name MkInput を取る。MkForm の field は MkInput (text) /
    // MkTextarea / MkSwitch (checkbox) で、name = type='text' の input。
    // 複数 text input がある場合 dialog 内が最後 (= unshift order) なので
    // type === 'text' で filter して最後を選ぶ。
    await page.evaluate((n) => {
      const inputs = (
        Array.from(document.querySelectorAll('input')) as HTMLInputElement[]
      ).filter((i) => i.type === 'text');
      const target = inputs[inputs.length - 1];
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, n);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, clipName);

    // clips/create response を捕捉して "Done" click
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/clips/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    // MkModalWindow の ok button (i18n.ts.done = "Done")
    await clickButtonContainingText(page, 'Done');
    const created = await createResp;
    const body = await created.json();
    expect(body.id).toBeTruthy();
    expect(body.name).toBe(clipName);
  });
});
