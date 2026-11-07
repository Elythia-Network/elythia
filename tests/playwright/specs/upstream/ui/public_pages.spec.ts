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
//
// /channels と /tags/:tag は、サインインした利用者の操作も通す (#4072)。チャンネルは検索のタブで
// 名前を検索して結果に出ること、タグはそのタグで投稿するボタンから投稿して本文にタグが付くこと。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { cleanupApi } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { pageBody } from '../../../fixtures/route_check';
import { type RootFixture, uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';
import { setPostText, submitPostForm, waitForPostForm } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser } from '../../../fixtures/ui_timeline';

test.describe('UI: public SPA pages with API hydration', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(30_000);

  test('navigate to /search and search input is rendered', coversRoutes('/search'), async ({ page, baseURL, request }) => {
    // search.vue は `notesSearchAvailable` (= 未ログイン時は
    // instance.policies.canSearchNotes) が false だと input の代わりに
    // "Note search is unavailable." を出す。canSearchNotes は upstream の
    // DEFAULT_POLICIES でも false なので、default policy を立ててから開く。
    // update-default-policies は既定のポリシーを丸ごと置き換えるので、今の値に
    // canSearchNotes だけを足して送り、終わったら控えた値に戻す (#4066。以前は
    // `{ canSearchNotes: false }` だけを送り、他の既定のポリシーを消していた)
    const before = await callApi(request, 'admin/meta', { i: root.token });
    expect(before.ok(), 'admin/meta を読めなかった').toBe(true);
    const original = (await before.json()).policies as Record<string, unknown>;
    expect(original, '既定のポリシー').toBeTruthy();
    try {
      const updated = await callApi(request, 'admin/roles/update-default-policies', {
        i: root.token,
        policies: { ...original, canSearchNotes: true },
      });
      expect(updated.ok(), '既定のポリシーを変えられない').toBe(true);

      await page.setViewportSize({ width: 1600, height: 900 });
      await gotoRoute(page, baseURL!, '/search');

      // search page には input field が必ず render される
      await page.waitForFunction(
        () => document.querySelectorAll('input').length > 0,
        { timeout: 20_000 },
      );
    } finally {
      // 他 spec に policy を持ち越さない
      const restored = await callApi(request, 'admin/roles/update-default-policies', { i: root.token, policies: original });
      expect.soft(restored.ok(), '既定のポリシーを元に戻せなかった').toBe(true);
    }
  });

  test('navigate to /channels (public channel list)', coversRoutes('/channels'), async ({ page, baseURL }) => {
    await page.setViewportSize({ width: 1600, height: 900 });
    await gotoRoute(page, baseURL!, '/channels');

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

  test('navigate to /tags/test (hashtag page)', coversRoutes('/tags/:tag'), async ({ page, baseURL }) => {
    await page.setViewportSize({ width: 1600, height: 900 });
    await gotoRoute(page, baseURL!, '/tags/test');

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

  test('search channels by name on the search tab', coversRoutes('/channels'), async ({ page, baseURL, request }) => {
    const me = await freshUser(request, 'pwch');
    const name = `pwchannel${Date.now().toString(36)}`;
    const channel = await apiOk(request, 'channels/create', { i: root.token, name, description: 'playwright' }) as { id: string };
    try {
      await uiSigninAs(page, request, baseURL!, me);
      await gotoRoute(page, baseURL!, '/channels');
      // 検索のタブを開くまで、送信の「Search」のボタンは無い
      await clickButtonContainingText(page, 'Search');
      const input = pageBody(page).locator('input[type="search"]');
      await input.fill(name);
      const searched = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/channels/search' && r.request().postDataJSON()?.query === name, { timeout: 15_000 });
      await input.press('Enter');
      expect((await searched).ok(), 'channels/search が失敗した').toBe(true);
      await expect(pageBody(page).locator(`a[href="/channels/${channel.id}"]`).first(), '検索の結果にチャンネルが出ない').toBeVisible({ timeout: 15_000 });
    } finally {
      await cleanupApi(request, 'channels/update', { i: root.token, channelId: channel.id, isArchived: true });
    }
  });

  test('post to a hashtag from its page', coversRoutes('/tags/:tag'), async ({ page, baseURL, request }) => {
    const me = await freshUser(request, 'pwtag');
    const tag = `pwtag${Date.now().toString(36)}`;
    await uiSigninAs(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, `/tags/${tag}`);
    await clickButtonContainingText(page, 'Post to this hashtag');
    await waitForPostForm(page);
    await setPostText(page, 'hello');
    const sent = await submitPostForm(page);
    expect(String(sent.text), '投稿にタグが付かない').toContain(`#${tag}`);
    await expect.poll(async () => (await apiOk(request, 'notes/search-by-tag', { tag, limit: 10 }) as unknown[]).length, { message: 'タグの一覧に投稿が出ない' }).toBe(1);
  });
});
