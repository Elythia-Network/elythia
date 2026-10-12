/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/profile で name MkInput を編集 → manualSave button click →
// /api/i/update + /api/i 両方で新 name が反映されることを verify する
// **真の write-flow** spec。
//
// MkInput (manualSave) は input 値変更で `<MkButton :class="$style.save">`
// を表示する。click すると updated event → os.apiWithDialog('i/update', ...)
// で name が persist される。本 spec は i/update 応答 + /api/i の 2 段で
// round-trip を verify する (= auth middleware tokenCache が i/update 後に
// 正しく invalidate されることを backend regression test として担保、#960)。
//
// 注意: /settings/* は親 layout の MkSuperMenu に search MkInput (type=search)
// があり、page 全体 input[0] はこの search box。form 本体の name input を
// 取るには `i.type === "text"` filter が必要 (MkInput type prop default は
// 暗黙の "text" で type 属性を render しないため input[type="text"] では
// match しない、#744 batch3)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /settings/profile name save flow', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('edit name MkInput → click save → i/update + /api/i both reflect new name', async ({
    page,
    baseURL,
    request,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/profile`, { waitUntil: 'domcontentloaded' });

    // name MkInput の <input> 要素を待つ。MkInput は type prop default が
    // 暗黙の "text" (HTML 仕様) で type 属性を render しないため、CSS
    // selector の input[type="text"] では match しない。代わりに JS
    // input.type === "text" を見て filter する (HTMLInputElement.type は
    // type 属性が空でも default で "text" を返す, #744 batch3)。
    await page.waitForFunction(
      () => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.type === 'text');
      },
      { timeout: 20_000 },
    );

    const newName = `pwname-${Date.now().toString().slice(-9)}`;
    // 最初の text input (= profile.name) に native value setter で書き込み。
    // post_note.spec.ts と同 pattern (Vue v-model に input event を届ける)。
    await page.evaluate((n) => {
      const inputs = (Array.from(document.querySelectorAll('input')) as HTMLInputElement[]).filter(
        (i) => i.type === 'text',
      );
      const target = inputs[0];
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, n);
      target.dispatchEvent(new Event('input', { bubbles: true }));
      target.dispatchEvent(new Event('change', { bubbles: true }));
    }, newName);

    // manualSave button が出現するまで待つ。MkInput は value 変更で changed
    // computed が true になり save button を mount する。
    await page.waitForFunction(
      () => {
        // save button は MkButton primary。MkInput 内の最初の MkButton
        // (= save) を含む。CSS class $style.save は scoped で predict 困難
        // なので、profile.name の MkInput container に紐づいた button を
        // 探す。簡易には body 内 "Save" button text で取る。
        const btns = Array.from(document.querySelectorAll('button'));
        return btns.some((b) => (b.textContent ?? '').includes('Save'));
      },
      { timeout: 10_000 },
    );

    // i/update response を捕捉して save click
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() === 200,
      { timeout: 15_000 },
    );
    // 最初の "Save" button を click (= name MkInput の save)
    await clickButtonContainingText(page, 'Save');
    // i/update 応答 body は更新後 user object を返す (handler が fresh fetch)。
    const update = await updateResp;
    const updateBody = await update.json();
    expect(updateBody.name).toBe(newName);

    // 同じ token で /api/i を読み直す。i/update は auth middleware の
    // tokenCache を invalidate するため (#960)、cache TTL (30s) 内でも
    // fresh user が返る。
    const meResp = await callApi(request, 'i', { i: root.token });
    expect(meResp.status()).toBe(200);
    const me = await meResp.json();
    expect(me.name).toBe(newName);
  });
});
