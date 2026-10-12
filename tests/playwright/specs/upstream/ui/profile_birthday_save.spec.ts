/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/profile で birthday MkInput (type="date", manualSave) を編集
// → save click → /api/i/update + /api/i 両方で新 birthday が反映されることを
// verify する write-flow spec。
//
// /settings/profile 内の type="date" input は唯一 profile.birthday なので
// `i.type === 'date'` filter で曖昧性なく当てられる。manualSave なので
// 値変更後に "Save" button が出現する流れは profile_name_save と同 pattern。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /settings/profile birthday save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('edit birthday → save → i/update + /api/i both reflect', async ({
    page,
    baseURL,
    request,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/profile`, { waitUntil: 'domcontentloaded' });

    // birthday input (type="date") が hydrate するまで待つ
    await page.waitForFunction(
      () => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.type === 'date');
      },
      { timeout: 20_000 },
    );

    // YYYY-MM-DD format。run ごとに変わる値で実機 round-trip を担保する
    // (連続 run で同値だと "未変更" 判定で MkInput の Save button が出ず
    // timeout する)。秒単位の resolution で 1990 〜 2010 範囲をズラすので、
    // 同秒内連続 run でない限り値が衝突しない。
    const seconds = Math.floor(Date.now() / 1000);
    const dayInRange = seconds % 365;
    const year = 1990 + (seconds % 21);
    const baseDate = new Date(year, 0, 1);
    baseDate.setDate(baseDate.getDate() + dayInRange);
    const newBirthday = `${baseDate.getFullYear()}-${String(baseDate.getMonth() + 1).padStart(2, '0')}-${String(baseDate.getDate()).padStart(2, '0')}`;

    await page.evaluate((b) => {
      const inputs = (Array.from(document.querySelectorAll('input')) as HTMLInputElement[]).filter(
        (i) => i.type === 'date',
      );
      const target = inputs[0];
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, b);
      target.dispatchEvent(new Event('input', { bubbles: true }));
      target.dispatchEvent(new Event('change', { bubbles: true }));
    }, newBirthday);

    // manualSave button の出現を待つ
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button'));
        return btns.some((b) => (b.textContent ?? '').includes('Save'));
      },
      { timeout: 10_000 },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const update = await updateResp;
    const updateBody = await update.json();
    expect(updateBody.birthday).toBe(newBirthday);

    // /api/i でも反映されること (= tokenCache invalidation, #960)
    const meResp = await callApi(request, 'i', { i: root.token });
    expect(meResp.status()).toBe(200);
    const me = await meResp.json();
    expect(me.birthday).toBe(newBirthday);
  });
});
