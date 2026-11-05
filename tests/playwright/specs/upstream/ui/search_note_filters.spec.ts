/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ノート検索 (/search) で、キーワード・利用者・サーバーで絞ると、その条件が
// notes/search に送られ、該当するノートだけが出ることを確かめる (#4049)。
//
// テストの stack は Meilisearch を持たない (instance.yml)。検索は DB の検索
// (sqlLike) で、noteSearchableScope は global になる (internal/api/meta の
// NoteSearchableScope)。ノート検索は既定のポリシーでは使えない (canSearchNotes が
// false) ので、検索する人にだけロールで許す。既定のポリシーを書き換えると meta を
// 共有するほかの spec に響くため。ロールは finally で消す。
//
// テストの stack は連合を切ってある (meta の federation が none) ので、検索の範囲に
// 「サーバーを指定」が出ない (search.note.vue)。この spec の間だけ all にし、前の値に
// finally で戻す (meta を書き換えるので、shard-plan.mjs が meta の project に入れる)。
//
// 出ないことは、検索の応答が返り、出るはずのノートが出た後に確かめる。サーバーで
// 絞ったときは出るはずのノートが無いので、応答の件数 (0) と、表示が空になることで見る。

import { readFileSync } from 'node:fs';
import { expect, type Page, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { pageBody } from '../../../fixtures/route_check';
import { type RootFixture, uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickWhenReady } from '../../../fixtures/ui_click';
import { fillInputByLabel } from '../../../fixtures/ui_input';
import { clickOnlyTestId } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

// MkRadios の項目を、見出しの文字 (完全一致、ちょうど 1 つ) で押す
function clickRadio(page: Page, label: string): Promise<void> {
  return clickWhenReady(
    page,
    `ラジオの項目「${label}」(ちょうど 1 つ)`,
    (l: string) => {
      const hits = Array.from(document.querySelectorAll('[role="checkbox"]'))
        .filter((o) => o.querySelector('input[type="radio"]') !== null && (o.textContent ?? '').trim() === l);
      return hits.length === 1 ? hits[0] : null;
    },
    label,
  );
}

// 検索ボタンを押し、送った本文と、返ったノートの id を返す
async function runSearch(page: Page): Promise<{ body: Record<string, unknown>; ids: string[] }> {
  const req = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === '/api/notes/search', { timeout: 15_000 });
  const resp = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/notes/search', { timeout: 15_000 });
  await clickButtonByText(page, 'Search');
  const body = (await req).postDataJSON() as Record<string, unknown>;
  const r = await resp;
  expect(r.status(), `notes/search failed: ${await r.text()}`).toBe(200);
  const ids = ((await r.json()) as { id: string }[]).map((n) => n.id);
  return { body, ids };
}

test.describe('UI: note search filters', () => {
  test.setTimeout(120_000);
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('keyword, user, and server filters are sent to notes/search and narrow the results', coversRoutes('/search'), async ({ page, baseURL, request }) => {
    const searcher = await signupUser(request, randomUsername('pwsrch'));
    const alice = await signupUser(request, randomUsername('pwsrcha'));
    const bob = await signupUser(request, randomUsername('pwsrchb'));
    const keyword = `pwkw${Date.now()}`;
    const a1 = await createNote(request, alice.token, { text: `${keyword} alpha` });
    const a2 = await createNote(request, alice.token, { text: `${keyword} beta` });
    const b1 = await createNote(request, bob.token, { text: `${keyword} gamma` });
    const other = await createNote(request, alice.token, { text: `pwother${Date.now()} delta` });

    const metaBefore = await callApi(request, 'admin/meta', { i: root.token });
    expect(metaBefore.ok(), 'admin/meta を読めなかった').toBe(true);
    const federationBefore = (await metaBefore.json()).federation as string;
    expect(typeof federationBefore).toBe('string');

    const roleResp = await callApi(request, 'admin/roles/create', {
      i: root.token, name: `pw-search-${Date.now()}`, description: 'note search spec', color: null, iconUrl: null,
      target: 'manual', condFormula: {}, isPublic: false, isExplorable: false,
      isAdministrator: false, isModerator: false, asBadge: false,
      canEditMembersByModerator: false, displayOrder: 0,
      policies: { canSearchNotes: { useDefault: false, priority: 0, value: true } },
    });
    expect(roleResp.status(), `admin/roles/create failed: ${await roleResp.text()}`).toBe(200);
    const roleId = (await roleResp.json()).id as string;
    try {
      const fed = await callApi(request, 'admin/update-meta', { i: root.token, federation: 'all' });
      expect(fed.status(), `admin/update-meta failed: ${await fed.text()}`).toBe(204);
      const assigned = await callApi(request, 'admin/roles/assign', { i: root.token, roleId, userId: searcher.id });
      expect(assigned.status()).toBe(204);

      await uiSigninAs(page, request, baseURL!, searcher);
      await gotoRoute(page, baseURL!, '/search');
      const body = pageBody(page);
      const query = body.locator('input[type="search"]');
      await expect(query).toHaveCount(1, { timeout: 15_000 });
      await query.fill(keyword);

      // キーワードだけ (このサーバー): キーワードを含む 3 件が出て、含まないノートは出ない
      await clickRadio(page, 'Local');
      const local = await runSearch(page);
      expect(local.body).toMatchObject({ query: keyword, host: '.' });
      expect(local.body.userId).toBeUndefined();
      expect(new Set(local.ids)).toEqual(new Set([a1.id, a2.id, b1.id]));
      for (const n of [a1, a2, b1]) await expect(body.getByText(n.text!, { exact: true })).toBeVisible({ timeout: 15_000 });
      await expect(body.getByText(other.text!, { exact: true })).toHaveCount(0);

      // 利用者で絞る: 選んだ人のノートだけになる
      await clickRadio(page, 'Specific user');
      await clickButtonByText(page, 'Select a user');
      await fillInputByLabel(page, 'Username', alice.username);
      await clickWhenReady(
        page,
        `利用者の選択の候補 @${alice.username}`,
        (u: string) => {
          const rows = Array.from(document.querySelectorAll('._button')).filter((el) => (el.textContent ?? '').includes(`@${u}`) && el.querySelector('img, ._avatar, [class*="avatar"]') !== null);
          return rows.length === 1 ? rows[0] : null;
        },
        alice.username,
      );
      await clickOnlyTestId(page, 'modal-window-ok');
      const byUser = await runSearch(page);
      expect(byUser.body).toMatchObject({ query: keyword, userId: alice.id, host: '.' });
      expect(new Set(byUser.ids)).toEqual(new Set([a1.id, a2.id]));
      for (const n of [a1, a2]) await expect(body.getByText(n.text!, { exact: true })).toBeVisible({ timeout: 15_000 });
      await expect(body.getByText(b1.text!, { exact: true })).toHaveCount(0);

      // サーバーで絞る: 別のサーバーを指定すると、このサーバーのノートは出ない
      await clickRadio(page, 'Specific server');
      await fillInputByLabel(page, 'Enter the server host', 'remote.example');
      const byHost = await runSearch(page);
      expect(byHost.body).toMatchObject({ query: keyword, host: 'remote.example' });
      expect(byHost.body.userId).toBeUndefined();
      expect(byHost.ids).toEqual([]);
      await expect(body.getByText('No notes', { exact: true })).toBeVisible({ timeout: 15_000 });
      for (const n of [a1, a2, b1]) await expect(body.getByText(n.text!, { exact: true })).toHaveCount(0);
    } finally {
      const restored = await callApi(request, 'admin/update-meta', { i: root.token, federation: federationBefore });
      await callApi(request, 'admin/roles/delete', { i: root.token, roleId });
      expect(restored.status(), 'federation を戻せなかった').toBe(204);
    }
  });
});
