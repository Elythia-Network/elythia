/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// public 認証不要 SPA page の navigation smoke。
//
// frontend_routes.spec.ts は logged-out home / about / explore / signup 等の
// 「signup wall を含む」route 群を navigate する smoke。本 spec は API 結果
// を含む public route (= /search / /channels / /tags 等) を navigate して、
// SPA component が API 応答を受けて render することを verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import type { RootFixture } from '../../../fixtures/ui_auth';

test.describe('UI: public SPA pages with API hydration', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(30_000);

  test('navigate to /search and search input is rendered', async ({ page, baseURL, request }) => {
    // search.vue は `notesSearchAvailable` (= 未ログイン時は
    // instance.policies.canSearchNotes) が false だと input の代わりに
    // "Note search is unavailable." を出す。canSearchNotes は upstream の
    // DEFAULT_POLICIES でも false なので、default policy を立ててから開く。
    await callApi(request, 'admin/roles/update-default-policies', {
      i: root.token,
      policies: { canSearchNotes: true },
    });

    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/search`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // search page には input field が必ず render される
    await page.waitForFunction(
      () => document.querySelectorAll('input').length > 0,
      { timeout: 20_000 },
    );

    // 他 spec に policy を持ち越さない
    await callApi(request, 'admin/roles/update-default-policies', {
      i: root.token,
      policies: { canSearchNotes: false },
    });
  });

  test('navigate to /channels (public channel list)', async ({ page, baseURL }) => {
    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/channels`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // channels page は MkPagination + tab buttons (Trending / Featured /
    // Following / Owned / Search) を mount する。logged-out でも tab 切替
    // 不能なだけで button 自体は render される。<h1> + button 1 つ以上を
    // 確認すれば「PageWithHeader + tabs hydrate 完了」のサインになる。
    await page.waitForFunction(
      () =>
        document.querySelector('h1') !== null && document.querySelectorAll('button').length > 0,
      { timeout: 20_000 },
    );
  });

  test('navigate to /tags/test (hashtag page)', async ({ page, baseURL }) => {
    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/tags/test`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // logged-out 状態の /tags/:tag は upstream Misskey が visitor dashboard
    // fallback (= sign-up wall + local timeline preview) に流す挙動なので、
    // hashtag header element は render されない。本 spec の意図は「SPA route
    // が 200 を返して何らかの content が hydrate される」までの smoke で、
    // hashtag 固有の hydration verify は authenticated 視点が必要なため別
    // spec の scope (route_matrix で navbar smoke 経由で間接 cover)。
    await page.waitForFunction(
      () => (document.body.textContent?.length ?? 0) > 100,
      { timeout: 20_000 },
    );
  });
});
