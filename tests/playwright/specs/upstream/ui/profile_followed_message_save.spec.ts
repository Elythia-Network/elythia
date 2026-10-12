/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/profile で followedMessage MkInput (manualSave 3rd text input) を
// 編集 → save click → /api/i/update + /api/i 両方で新 followedMessage が反映
// されることを verify する write-flow spec。
//
// /settings/profile の type=text 入力順 (collapsed folder 前提):
//   inputs[0] = profile.name
//   inputs[1] = profile.location
//   inputs[2] = profile.followedMessage  ← 本 spec の対象
// fields metadata は MkFolder 内 (デフォルト folded) なので index は安定。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /settings/profile followedMessage save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('edit followedMessage → save → i/update + /api/i both reflect', async ({
    page,
    baseURL,
    request,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/profile`, { waitUntil: 'domcontentloaded' });

    await page.waitForFunction(
      () => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.filter((i) => i.type === 'text').length >= 3;
      },
      { timeout: 20_000 },
    );

    const newMsg = `pwfm-${Date.now().toString().slice(-9)}`;
    await page.evaluate((msg) => {
      const inputs = (Array.from(document.querySelectorAll('input')) as HTMLInputElement[]).filter(
        (i) => i.type === 'text',
      );
      const target = inputs[2];
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, msg);
      target.dispatchEvent(new Event('input', { bubbles: true }));
      target.dispatchEvent(new Event('change', { bubbles: true }));
    }, newMsg);

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
    expect(updateBody.followedMessage).toBe(newMsg);

    const meResp = await callApi(request, 'i', { i: root.token });
    expect(meResp.status()).toBe(200);
    const me = await meResp.json();
    expect(me.followedMessage).toBe(newMsg);
  });
});
