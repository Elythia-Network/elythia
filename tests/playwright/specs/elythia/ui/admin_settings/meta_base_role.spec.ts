/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の設定の表 (#4066): 基本ロール (既定のポリシー)。/admin/roles の「Role template」の
// 枠で値を変えて保存し (admin/roles/update-default-policies)、ロールを持たない新しい利用者で
// 効き目を見る。既定のポリシーは全 spec が共有するので、行の後は控えた値に戻す (この spec は
// update-default-policies を呼ぶので meta の project で流れる)。ポリシーごとの効き目は
// policy_*.spec.ts の表で見ているので、ここでは基本ロールの経路を 2 行で確かめる。

import { type APIRequestContext, expect, type Page, test } from '@playwright/test';
import { type AdminRowContext, coversPolicies, expectApiError, expectApiOk, openPolicyFolder, rootFixture, setSwitch, signinAdmin, switchIn } from '../../../../fixtures/admin_table';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { waitForMountedPage } from '../../../../fixtures/route_check';
import { checkEffect, expectEffectFails, MUTATION, skipOnTsBackend } from '../../../../fixtures/settings_table';
import { pageBody } from '../../../../fixtures/ui_timeline';
import { coversRoutes, gotoRoute } from '../../../../fixtures/ui_route';

interface BaseRow {
  key: string;
  on: boolean;
  effect: (ctx: AdminRowContext) => Promise<void>;
}

const ROWS: BaseRow[] = [
  {
    key: 'canSearchNotes', on: true,
    effect: async ({ request, user }) => { await expectApiOk(request, 'notes/search', { i: user.token, query: 'pw' }); },
  },
  {
    key: 'gtlAvailable', on: false,
    effect: ({ request, user }) => expectApiError(request, 'notes/global-timeline', { i: user.token }, 'GTL_DISABLED'),
  },
];

/** The base role folder ("Role template"), opened. */
async function baseRoleFolder(page: Page) {
  const header = pageBody(page).getByTestId('folder-header').filter({ has: page.getByText('Role template', { exact: true }) });
  await expect(header, '「Role template」の枠').toHaveCount(1, { timeout: 20_000 });
  const group = header.locator('xpath=ancestor::*[@role="group"][1]');
  if ((await group.getAttribute('aria-expanded')) !== 'true') await header.click();
  return group;
}

async function runBaseRow(page: Page, request: APIRequestContext, baseURL: string, row: BaseRow): Promise<void> {
  skipOnTsBackend();
  const root = rootFixture();
  const original = (await expectApiOk(request, 'admin/meta', { i: root.token })).policies as Record<string, unknown>;
  expect(original, '既定のポリシー').toBeTruthy();
  // 既定値と逆にする行なので、始める前の値が行の値と同じだと、変えずに通ってしまう
  expect(original[row.key], `${row.key} がすでに ${row.on} になっている`).toBe(!row.on);
  const user = await signupUser(request, randomUsername('pwbase'));
  const ctx: AdminRowContext = { page, request, baseURL, user, root };
  try {
    await signinAdmin(page, baseURL, root);
    await gotoRoute(page, baseURL, '/admin/roles');
    await waitForMountedPage(page);
    if (!MUTATION) {
      await baseRoleFolder(page);
      await setSwitch(await switchIn(await openPolicyFolder(page, row.key), 'Enable'), row.on);
      const saved = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/roles/update-default-policies', { timeout: 15_000 });
      await (await baseRoleFolder(page)).locator('button').filter({ hasText: /^\s*Save\s*$/ }).last().click();
      expect((await saved).ok(), 'admin/roles/update-default-policies が失敗した').toBe(true);
      await expect.poll(async () => (await expectApiOk(request, 'admin/meta', { i: root.token })).policies?.[row.key], { message: `${row.key} が保存されない` }).toBe(row.on);
    }
    await page.reload({ waitUntil: 'domcontentloaded' });
    await waitForMountedPage(page);
    await baseRoleFolder(page);
    const folder = await openPolicyFolder(page, row.key);
    if (MUTATION) {
      await expectEffectFails(row.key, row.effect, ctx);
      return;
    }
    await expect(await switchIn(folder, 'Enable')).toHaveAttribute('data-checked', String(row.on));
    await checkEffect(row.key, row.effect, undefined, ctx);
  } finally {
    const restored = await callApi(request, 'admin/roles/update-default-policies', { i: root.token, policies: original });
    expect.soft(restored.ok(), `${row.key}: 既定のポリシーを元に戻せなかった`).toBe(true);
  }
}

test.describe('admin: base role (default policies)', () => {
  test.setTimeout(60_000);
  for (const row of ROWS) {
    test(`base role: ${row.key} → ${row.on}`, { annotation: [...coversRoutes('/admin/roles').annotation, ...coversPolicies(row.key).annotation] }, ({ page, request, baseURL }) => runBaseRow(page, request, baseURL!, row));
  }
});
