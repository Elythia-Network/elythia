/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 探す (/explore) の利用者のタブで、欄ごとに並び替えの条件が users に送られ、
// 「新しく参加した利用者」は登録の新しい順、「最近活動した利用者」は活動の新しい順に
// 並ぶこと、ロールのタブに公開のロールが出て、選ぶとそのロールの利用者が出ることを
// 確かめる (#4049)。
//
// 並び順は、同じ 2 人が 2 つの欄で逆の順に並ぶことで見る。後から登録した人が先に
// 投稿し、先に登録した人が後で投稿する。どちらの欄も新しいものが先頭なので、
// 片方だけで見ると「作った順に並べただけ」でも通ってしまう。
//
// 両方の欄は state: 'alive' などで絞るので、投稿させて活動中にする
// (explore_users_render.spec.ts と同じ理由)。ほかの spec は直列に走るので、
// 開いた時点ではこの 2 人がいちばん新しい。
//
// ロールは finally で消す。

import { readFileSync } from 'node:fs';
import { expect, type Locator, type Page, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { pageBody } from '../../../fixtures/route_check';
import { type RootFixture, uiSigninAs } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';
import { waitForApiBody } from '../../../fixtures/ui_post_form';
import { coversRoutes, expectRoutedPage, gotoRoute } from '../../../fixtures/ui_route';

// 見出しが title の MkFoldableSection (見出しの header の親) を返す
function section(page: Page, title: string): Locator {
  return pageBody(page).locator('header', { hasText: title }).locator('xpath=..');
}

// 欄の中の利用者のカード (_panel) を並んでいる順に読み、candidates のどれか
// (`@<username>` を含むもの) に当てる。どれでもないカードは '?' にする
// (自己紹介の文が username の直後に続くので、カードの文字から username を切り出さない)
function usernamesIn(sec: Locator, candidates: string[]): Promise<string[]> {
  return sec.locator('._panel').evaluateAll((cards, names) =>
    cards.map((c) => names.find((n) => (c.textContent ?? '').includes(`@${n}`)) ?? '?'), candidates);
}

test.describe('UI: explore users sorting and roles tab', () => {
  test.setTimeout(90_000);
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('sections are sorted by their own key, and the roles tab lists public roles', coversRoutes('/explore'), async ({ page, baseURL, request }) => {
    const viewer = await signupUser(request, randomUsername('pwexpv'));
    const older = await signupUser(request, randomUsername('pwexpo'));
    const newer = await signupUser(request, randomUsername('pwexpn'));
    for (const u of [newer, older]) {
      const r = await callApi(request, 'notes/create', { i: u.token, text: `pw-explore ${u.username}`, visibility: 'public' });
      expect(r.status()).toBe(200);
    }

    const roleName = `pw-explore-role-${Date.now()}`;
    const roleResp = await callApi(request, 'admin/roles/create', {
      i: root.token, name: roleName, description: 'explore spec', color: null, iconUrl: null,
      target: 'manual', condFormula: {}, isPublic: true, isExplorable: true,
      isAdministrator: false, isModerator: false, asBadge: false,
      canEditMembersByModerator: false, displayOrder: 0, policies: {},
    });
    expect(roleResp.status(), `admin/roles/create failed: ${await roleResp.text()}`).toBe(200);
    const roleId = (await roleResp.json()).id as string;
    try {
      expect((await callApi(request, 'admin/roles/assign', { i: root.token, roleId, userId: older.id })).status()).toBe(204);

      await uiSigninAs(page, request, baseURL!, viewer);
      const sorts: Record<string, unknown>[] = [];
      page.on('request', (r) => {
        if (r.method() === 'POST' && new URL(r.url()).pathname === '/api/users') sorts.push(r.postDataJSON() as Record<string, unknown>);
      });
      await gotoRoute(page, baseURL!, '/explore#users');

      const joined = section(page, 'Newly joined users');
      const active = section(page, 'Recently active users');
      await expect(joined).toHaveCount(1, { timeout: 15_000 });
      await expect(active).toHaveCount(1);
      await expect.poll(async () => (await usernamesIn(joined, [older.username, newer.username])).slice(0, 2), { timeout: 15_000 }).toEqual([newer.username, older.username]);
      await expect.poll(async () => (await usernamesIn(active, [older.username, newer.username])).slice(0, 2), { timeout: 15_000 }).toEqual([older.username, newer.username]);
      expect(sorts).toContainEqual(expect.objectContaining({ origin: 'local', state: 'alive', sort: '+createdAt' }));
      expect(sorts).toContainEqual(expect.objectContaining({ origin: 'local', sort: '+updatedAt' }));

      // ロールのタブ: 作ったロールが出て、選ぶとそのロールの利用者が出る
      const rolesListed = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/roles/list', { timeout: 15_000 });
      await clickWhenReady(page, 'タブ「Roles」', () =>
        Array.from(document.querySelectorAll('button')).find((b) => (b.textContent ?? '').trim() === 'Roles' && b.querySelector('i.ti-badges') !== null));
      await rolesListed;
      const roleLink = pageBody(page).getByText(roleName, { exact: true });
      await expect(roleLink).toBeVisible({ timeout: 15_000 });
      const members = waitForApiBody(page, 'roles/users');
      await roleLink.click();
      expect((await members).roleId).toBe(roleId);
      await expect(page).toHaveURL(new RegExp(`/roles/${roleId}$`), { timeout: 15_000 });
      await expectRoutedPage(page, `/roles/${roleId}`);
      await expect(pageBody(page).getByText(`@${older.username}`, { exact: false }).first()).toBeVisible({ timeout: 15_000 });
      await expect(pageBody(page).getByText(`@${newer.username}`, { exact: false })).toHaveCount(0);
    } finally {
      await callApi(request, 'admin/roles/delete', { i: root.token, roleId });
    }
  });
});
