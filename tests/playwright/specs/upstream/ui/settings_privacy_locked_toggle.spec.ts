/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/privacy で isLocked switch を click → /api/i/update が
// {isLocked: true} で round-trip する **真の write-flow** spec。
//
// MkSwitch は <input type="checkbox" @click="toggle"> として render され、
// `@update:modelValue="save()"` で /api/i/update を即時呼ぶ。本 spec は
// 1 つ目の MkSwitch (isLocked) を click して API 起動を verify する。
//
// 注意: /settings/* は親 layout の MkSuperMenu に search MkInput
// (type=search) があり、page 全体 input[0] はそれ。form 本体の switch を
// 取るには `i.type === 'checkbox'` filter が必要 (#744 batch3 で発覚した
// MkInput type 暗黙 text と同 origin)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/privacy isLocked toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('toggle isLocked switch → /api/i/update round-trips', async ({
    page,
    baseURL,
    request,
  }) => {
    // 値 strict assertion を効かせるため、初期 state を false (= DB default)
    // に reset。prior run 累積で true のまま残っていると click で false に
    // 反転して期待と逆になるので、明示的に既知 state から始める。
    await callApi(request, 'i/update', { i: root.token, isLocked: false });

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/privacy`, { waitUntil: 'domcontentloaded' });

    // checkbox が hydrate するまで待つ
    await page.waitForFunction(
      () => document.querySelectorAll('input[type="checkbox"]').length >= 1,
      { timeout: 20_000 },
    );

    // i/update response を捕捉して isLocked switch を click
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() === 200,
      { timeout: 15_000 },
    );
    // 最初の checkbox = isLocked switch (privacy.vue:14)
    await clickWhenReady(page, 'isLocked の checkbox', () =>
      document.querySelector('input[type="checkbox"]'),
    );
    const update = await updateResp;
    const body = await update.json();
    // beforeAll の API reset で false から始まるので、click 後は必ず true。
    expect(body.isLocked).toBe(true);
  });
});
