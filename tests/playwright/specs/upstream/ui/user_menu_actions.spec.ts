/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 他人のプロフィールの「…」のメニューから付ける側の操作を通す (#4072)。外す側 (Unmute / Unblock) は
// user_unmute_via_menu / user_unblock_via_menu が見ている。
//
//   Mute           期間を選ぶダイアログで「One hour」を選び、約 1 時間後に切れるミュートになる
//   Mute Renotes   確認なしでリノートのミュートになる
//   Block          確認のダイアログを経てブロックになる
//   Add to list    子のメニューのリストの switch で、そのリストに入る
//   Add to antenna 子のメニューのアンテナ (利用者を指定する種類) に、その利用者が入る
//
// 鍵のアカウントへのフォローのボタンが申請になること、ブロックされている側からはフォローできず
// 関係が変わらないことも見る。結果はどれも API で照会する。

import { expect, type Page, test } from '@playwright/test';
import { pickSelect } from '../../../fixtures/admin_table';
import { type Principal } from '../../../fixtures/auth';
import { signinWithState } from '../../../fixtures/settings_table';
import { clickButtonByText, clickButtonContainingText, clickButtonWithIcon } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, clickMenuItem, freshUser } from '../../../fixtures/ui_timeline';

type Relation = { isMuted: boolean; isRenoteMuted: boolean; isBlocking: boolean; isFollowing: boolean; hasPendingFollowRequestFromYou: boolean };

async function relation(page: Page, me: Principal, target: Principal): Promise<Relation> {
  const [rel] = await apiOk(page.request, 'users/relation', { i: me.token, userId: [target.id] }) as Relation[];
  return rel;
}

/** Opens the "..." menu on the profile of `target`. */
async function openUserMenu(page: Page, baseURL: string, target: Principal): Promise<void> {
  await gotoRoute(page, baseURL, `/@${target.username}`);
  await expect(page.getByText(`@${target.username}`).first()).toBeVisible({ timeout: 20_000 });
  await clickButtonWithIcon(page, 'i.ti-dots');
}

/** Waits for the response of `endpoint` and checks it succeeded. */
function expectCall(page: Page, endpoint: string) {
  return page.waitForResponse((r) => new URL(r.url()).pathname === `/api/${endpoint}`, { timeout: 15_000 }).then((r) => {
    expect(r.ok(), `${endpoint} が失敗した`).toBe(true);
    return r;
  });
}

test.describe('UI: user menu actions', () => {
  test.setTimeout(60_000);

  let me: Principal;
  let target: Principal;
  test.beforeEach(async ({ page, request, baseURL }) => {
    me = await freshUser(request, 'pwumme');
    target = await freshUser(request, 'pwumt');
    await signinWithState(page, request, baseURL!, me);
  });

  test('mute for one hour', coversRoutes('/@:acct/:page?'), async ({ page, baseURL }) => {
    await openUserMenu(page, baseURL!, target);
    await clickMenuItem(page, 'Mute');
    // 期間の選択は確認のダイアログの中の MkSelect。見出しと OK を両方含む、いちばん近い枠に絞る
    const dialog = page.getByText('Mute duration').locator('xpath=ancestor::div[.//*[@data-testid="modal-dialog-ok"]][1]');
    await pickSelect(page, dialog, 'One hour');
    const muted = expectCall(page, 'mute/create');
    await page.getByTestId('modal-dialog-ok').click();
    const expiresAt = (await muted).request().postDataJSON()?.expiresAt as number;
    expect(Math.abs(expiresAt - (Date.now() + 3_600_000)), '1 時間後に切れるミュートにならない').toBeLessThan(120_000);
    expect((await relation(page, me, target)).isMuted, 'ミュートにならない').toBe(true);
  });

  test('mute renotes', coversRoutes('/@:acct/:page?'), async ({ page, baseURL }) => {
    await openUserMenu(page, baseURL!, target);
    const muted = expectCall(page, 'renote-mute/create');
    await clickMenuItem(page, 'Mute Renotes');
    await muted;
    expect((await relation(page, me, target)).isRenoteMuted, 'リノートのミュートにならない').toBe(true);
  });

  test('block after confirming', coversRoutes('/@:acct/:page?'), async ({ page, baseURL }) => {
    await openUserMenu(page, baseURL!, target);
    await clickMenuItem(page, 'Block');
    const blocked = expectCall(page, 'blocking/create');
    await page.getByTestId('modal-dialog-ok').click();
    await blocked;
    expect((await relation(page, me, target)).isBlocking, 'ブロックにならない').toBe(true);
  });

  test('add to a list', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    const name = `pw list ${Date.now().toString(36)}`;
    const list = await apiOk(request, 'users/lists/create', { i: me.token, name }) as { id: string };
    // メニューはリストの一覧を開いた時点で読むので、作ってから開く
    await openUserMenu(page, baseURL!, target);
    await clickMenuItem(page, 'Add to list');
    const pushed = expectCall(page, 'users/lists/push');
    await page.locator('[role="menu"] [role="menuitemcheckbox"]').filter({ hasText: name }).click();
    await pushed;
    const shown = await apiOk(request, 'users/lists/show', { i: me.token, listId: list.id }) as { userIds: string[] };
    expect(shown.userIds, 'リストに入らない').toContain(target.id);
  });

  test('add to an antenna', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    const name = `pw antenna ${Date.now().toString(36)}`;
    const antenna = await apiOk(request, 'antennas/create', {
      i: me.token, name, src: 'users', keywords: [['pw']], excludeKeywords: [], users: [], caseSensitive: false, withReplies: false, withFile: false,
    }) as { id: string };
    await openUserMenu(page, baseURL!, target);
    await clickMenuItem(page, 'Add to antenna');
    const updated = expectCall(page, 'antennas/update');
    await clickMenuItem(page, name);
    await updated;
    const shown = await apiOk(request, 'antennas/show', { i: me.token, antennaId: antenna.id }) as { users: string[] };
    expect(shown.users, 'アンテナに入らない').toContain(`@${target.username}`);
  });

  test('following a locked account sends a request', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    await apiOk(request, 'i/update', { i: target.token, isLocked: true });
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    const requested = expectCall(page, 'following/create');
    // MkFollowButton は Playwright の見える判定に入らないことがある (follow_toggle.spec.ts) ので、
    // 要素の click() で押す
    await clickButtonContainingText(page, 'Send follow request');
    // フォローは既定で確認を挟む (alwaysConfirmFollow)
    await page.getByTestId('modal-dialog-ok').click();
    await requested;
    await expect(page.locator('button').filter({ hasText: 'Follow request pending' }), 'ボタンが申請中にならない').toHaveCount(1);
    const rel = await relation(page, me, target);
    expect([rel.hasPendingFollowRequestFromYou, rel.isFollowing], '申請にならない').toEqual([true, false]);
  });

  test('a blocked user cannot follow', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    await apiOk(request, 'blocking/create', { i: target.token, userId: me.id });
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    const refused = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/following/create', { timeout: 15_000 });
    await clickButtonByText(page, 'Follow');
    await page.getByTestId('modal-dialog-ok').click();
    const r = await refused;
    expect(r.status(), 'ブロックされているのにフォローが通る').toBe(400);
    expect((await r.json() as { error?: { code?: string } }).error?.code).toBe('BLOCKED');
    const rel = await relation(page, me, target);
    expect([rel.isFollowing, rel.hasPendingFollowRequestFromYou], 'ブロックされているのに関係が変わった').toEqual([false, false]);
  });
});
