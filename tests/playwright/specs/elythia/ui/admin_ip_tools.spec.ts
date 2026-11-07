/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// IP アドレスの道具 (#3105 ほか) を画面で通す (#4072)。IP の記録を有効にして 2 人の利用者に
// API を呼ばせ、記録を作る (このスタックでは同じ runner から入るので同じ IP になる)。管理者は
// /admin/ip-search で IP から探し、利用者ページの関連アカウントのタブで同じ IP の利用者を探し、
// モデレーションのログの IP 照会のタブで、いま行った照会が記録されたことを見る。
//
// IP の記録は instance meta なので、始める前の値を控えて終わったら戻す (この spec は
// admin/update-meta を書くので meta の project で流れる)。照会には canSearchIpHistory が要るので、
// root にその権限のロールを足し、終わったら消す。

import { expect, test } from '@playwright/test';
import { assignRole, cleanupApi, expectApiOk, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { type Principal, randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('admin: IP tools', () => {
  test.skip(isTsBackend, 'IP の道具は Elythia だけにある');
  test.setTimeout(90_000);

  let original: unknown;
  let roleId: string;
  let a: Principal;
  let b: Principal;
  let ip: string;

  test.beforeEach(async ({ request }) => {
    // 前のテストの値が残ると、beforeEach が途中で落ちたときに afterEach が消し済みのものを触る
    original = undefined;
    roleId = '';
    const root = rootFixture();
    original = (await expectApiOk(request, 'admin/meta', { i: root.token })).enableIpLogging;
    await expectApiOk(request, 'admin/update-meta', { i: root.token, enableIpLogging: true });
    roleId = await assignRole(request, root, root.id, { policies: { canSearchIpHistory: true } });
    a = await signupUser(request, randomUsername('pwipa'));
    b = await signupUser(request, randomUsername('pwipb'));
    // 有効にした後の API の呼び出しで IP が記録される
    await expectApiOk(request, 'i', { i: a.token });
    await expectApiOk(request, 'i', { i: b.token });
    const ips = await expectApiOk(request, 'admin/get-user-ips', { i: root.token, userId: a.id }) as { ip: string }[];
    expect(ips.length, 'IP が記録されない (前提)').toBeGreaterThan(0);
    ip = ips[0].ip;
  });
  test.afterEach(async ({ request }) => {
    const root = rootFixture();
    if (roleId !== '') await cleanupApi(request, 'admin/roles/delete', { i: root.token, roleId });
    if (original === undefined) return;
    const restored = await callApi(request, 'admin/update-meta', { i: root.token, enableIpLogging: original });
    expect.soft(restored.ok(), 'IP の記録の設定を元に戻せなかった').toBe(true);
  });

  test('search by IP lists the accounts that used it', coversRoutes('/admin/ip-search'), async ({ page, baseURL }) => {
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/ip-search');
    await page.getByTestId('ip-search-input').locator('input').fill(ip);
    const searched = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/ip/accounts', { timeout: 15_000 });
    await page.getByTestId('ip-search-submit').click();
    expect((await searched).ok(), 'admin/ip/accounts が失敗した').toBe(true);
    for (const u of [a, b]) {
      await expect(page.locator(`[data-testid="ip-search-account"][data-user-id="${u.id}"]`), `${u.username} が出ない`).toHaveCount(1);
    }
  });

  test('related accounts of a user, and the lookups in the moderation log', coversRoutes('/admin/user/:userId', '/admin/modlog'), async ({ page, baseURL, request }) => {
    const root = rootFixture();
    await signinAdmin(page, baseURL!, root);
    // beforeEach の admin/get-user-ips も同じ利用者の照会としてログに残るので、押す前の行を控えて除く
    type Entry = { id: string; kind: string; targetUserId: string };
    const lookupLog = async () => (await expectApiOk(request, 'admin/ip/lookup-log', { i: root.token }) as { entries: Entry[] }).entries;
    const before = new Set((await lookupLog()).map((e) => e.id));
    await gotoRoute(page, baseURL!, `/admin/user/${a.id}`);
    await page.getByTestId('admin-user-tab-related-accounts').click();
    const searched = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/ip/related-accounts', { timeout: 15_000 });
    await page.getByTestId('ip-related-run').click();
    expect((await searched).ok(), 'admin/ip/related-accounts が失敗した').toBe(true);
    await expect(page.locator(`[data-testid="ip-related-account"][data-user-id="${b.id}"]`), '同じ IP の利用者が関連アカウントに出ない').toHaveCount(1);

    // いま行った関連アカウントの照会が、照会のログに残る
    const entry = (await lookupLog()).find((e) => !before.has(e.id) && e.kind === 'relatedAccounts' && e.targetUserId === a.id);
    expect(entry, '照会がログに残らない').toBeTruthy();
    await gotoRoute(page, baseURL!, '/admin/modlog');
    await page.getByTestId('modlog-tab-ip-lookup').click();
    await expect(page.locator(`[data-testid="ip-lookup-log-entry"][data-entry-id="${entry!.id}"]`), '照会のログの画面に出ない').toHaveCount(1);
  });
});
