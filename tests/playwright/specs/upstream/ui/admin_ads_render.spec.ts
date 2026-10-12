/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/ads page で admin/ad/create で作成した ad が render されることを
// verify する spec。
//
// /admin/ads は admin/ad/list で ad 一覧を取得し、各 ad を MkAd コンポ
// と MkInput (URL / imageUrl 編集 form) で render する。ad の URL 文字列
// が body に出るのを hydration sign にする。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/ads page renders created ad', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('admin/ad/create + /admin/ads renders ad URL', async ({ page, baseURL, request }) => {
    // 一意 host 名 (URL は MkInput type=url で render され、value 属性に
    // そのまま入るので body の textContent には出ない可能性がある。代わりに
    // memo で識別する。memo は Ad の任意メタデータ field で MkInput には
    // bind されないが、admin/ad/list レスポンスの memo が UI 上の <input>
    // value として render される)
    const adUrl = `https://playwright-${Date.now().toString().slice(-9)}.invalid/`;
    const memo = `pwad-memo-${Date.now().toString().slice(-9)}`;

    const createResp = await callApi(request, 'admin/ad/create', {
      i: root.token,
      url: adUrl,
      memo,
      place: 'square',
      priority: 'middle',
      ratio: 1,
      expiresAt: Date.now() + 24 * 60 * 60 * 1000,
      startsAt: Date.now(),
      imageUrl: 'https://example.invalid/playwright.png',
      dayOfWeek: 0,
    });
    expect(createResp.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/ads`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // 各 ad は <input type=url> の value に URL がセットされて render
    // される。body.textContent に出ない (input value は textContent に
    // 含まれない) ので、document.querySelector で <input> の value 検索。
    await page.waitForFunction(
      (u) => Array.from(document.querySelectorAll('input')).some((el) => el.value === u),
      adUrl,
      { timeout: 20_000 },
    );
  });
});
