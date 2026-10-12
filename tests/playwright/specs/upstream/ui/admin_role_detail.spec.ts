/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/roles/:id (ロールの詳細画面) をブラウザで操作する (#2441)。
//
// 既存の `admin_roles_render.spec.ts` は **一覧まで**で、個別ロールの画面は
// 未検証だった。付与されているユーザーの確認と解除はこの画面でしかできない。
//
// ロールの付与を誤ったまま解除できないと、権限が意図せず残り続ける。一覧が
// 出るだけでは気付けない。
//
// 解除ボタン (ti-x) は `unassign()` = popup menu を開くだけで、その中の項目が
// API を呼ぶ。/settings/mute-block の × と同じ作り。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

interface RoleFixture {
  id: string;
  name: string;
  memberId: string;
  memberUsername: string;
}

/** Create a manual role with one assigned member. */
async function createRoleWithMember(
  request: import('@playwright/test').APIRequestContext,
  root: RootFixture,
): Promise<RoleFixture> {
  const name = `pw-role-${Date.now().toString().slice(-9)}`;
  const created = await callApi(request, 'admin/roles/create', {
    i: root.token,
    name,
    description: 'playwright fixture',
    color: null,
    iconUrl: null,
    target: 'manual',
    condFormula: {},
    isPublic: false,
    isAdministrator: false,
    isModerator: false,
    isExplorable: false,
    asBadge: false,
    canEditMembersByModerator: false,
    displayOrder: 0,
    policies: {},
  });
  expect(created.status()).toBe(200);
  const role = (await created.json()) as { id: string };

  const member = await signupUser(request, randomUsername('rolemem'), DEFAULT_TEST_PASSWORD);
  const assigned = await callApi(request, 'admin/roles/assign', {
    i: root.token,
    roleId: role.id,
    userId: member.id,
  });
  expect(assigned.status()).toBe(204);

  return { id: role.id, name, memberId: member.id, memberUsername: member.username };
}

test.describe('UI: /admin/roles/:id role detail', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('ロール名と付与されているユーザーが表示される', async ({ page, baseURL, request }) => {
    const role = await createRoleWithMember(request, root);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/roles/${role.id}`, { waitUntil: 'domcontentloaded' });

    await expect(page.getByText(role.name, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
    // 誰に付いているか出ないと、誤付与に気付けない。
    await expect(page.getByText(role.memberUsername, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('× から付与を解除できる', async ({ page, baseURL, request }) => {
    const role = await createRoleWithMember(request, root);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/roles/${role.id}`, { waitUntil: 'domcontentloaded' });
    await expect(page.getByText(role.memberUsername, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });

    // 対象行は「username と × の両方を含む div の最も内側」で取る。
    const row = page
      .locator('div')
      .filter({ has: page.getByText(role.memberUsername, { exact: false }) })
      .filter({ has: page.locator('button:has(i.ti-x)') })
      .last();
    await row.locator('button:has(i.ti-x)').click();

    const unassigned = page.waitForResponse(
      (r) => r.url().includes('/api/admin/roles/unassign') && r.status() < 400,
      { timeout: 20_000 },
    );
    await page.getByText('Unassign', { exact: true }).first().click();
    await unassigned;

    // 画面から消えるだけでなく、実際に権限が外れている。
    await expect(async () => {
      const shown = await callApi(request, 'admin/roles/users', {
        i: root.token,
        roleId: role.id,
        limit: 100,
      });
      expect(shown.status()).toBe(200);
      const body = (await shown.json()) as Array<{ user: { id: string } }>;
      expect(body.some((u) => u.user.id === role.memberId)).toBe(false);
    }).toPass({ timeout: 15_000 });
  });
});
