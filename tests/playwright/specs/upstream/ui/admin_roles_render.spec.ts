/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/roles page で admin/roles/create で作成した role の name が
// MkRolePreview 経由で render されることを verify する spec。
//
// admin/roles ページは admin/roles/list で role 一覧を取得し、target 別
// (manual / conditional) に MkFoldableSection で render する。本 spec は
// manual role を 1 件作成 → /admin/roles を navigate → role.name が body
// に出るのを hydration sign にする。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/roles page renders created role', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('admin/roles/create + /admin/roles renders role name', async ({ page, baseURL, request }) => {
    const roleName = `pwrole-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'admin/roles/create', {
      i: root.token,
      name: roleName,
      description: 'playwright test role',
      color: null,
      iconUrl: null,
      target: 'manual',
      condFormula: {},
      isPublic: false,
      isAdministrator: false,
      isModerator: false,
      asBadge: false,
      canEditMembersByModerator: false,
      displayOrder: 0,
      policies: {},
    });
    expect(createResp.status()).toBe(200);
    const created = await createResp.json();
    expect(created.id).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/roles`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // /admin/roles は MkRolePreview で role.name を MkSpacer 内 text として
    // mount する。manual role の section は default で expanded されるが、
    // MkFoldableSection の 内部レンダ完了まで待ってから body 検索する。
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      roleName,
      { timeout: 20_000 },
    );
  });
});
