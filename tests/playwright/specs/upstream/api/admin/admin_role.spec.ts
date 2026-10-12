/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #826: admin/roles CRUD + assign / unassign + /api/i roles 反映。
//
// upstream Misskey TS と mk-go は両方とも:
//   - admin/roles/create { name, description?, isModerator?, isAdministrator?,
//     isPublic?, ... } で role を作成し entity を返す
//   - admin/roles/list で全 role を返す
//   - admin/roles/show { roleId } で single role を返す
//   - admin/roles/assign { userId, roleId } で role を付与 (204)
//   - 付与後 /api/i の `roles` array に role が反映される
//   - admin/roles/unassign で取り外し (204)
//   - admin/roles/delete で role を削除 (204)
//
// 本 spec は両 backend 共通で 1 role の lifecycle を round-trip:
//   1. admin が role 作成 → list / show で確認
//   2. target user に assign → target の /api/i で roles 反映
//   3. unassign → target の /api/i から消える
//   4. role delete → list から消える
//
// afterEach で role / role assignment を best-effort cleanup する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

interface AdminRole {
  id: string;
  name: string;
}

interface RoleRef {
  id?: string;
  name?: string;
}

test.describe('admin: role CRUD + assign + /api/i roles reflection', () => {
  let createdRoleID: string | undefined;
  let assignedUserID: string | undefined;
  let rootToken: string | undefined;

  test.beforeEach(() => {
    resetRateLimit();
  });

  test.afterEach(async ({ request }) => {
    if (rootToken && assignedUserID && createdRoleID) {
      await callApi(request, 'admin/roles/unassign', {
        i: rootToken,
        userId: assignedUserID,
        roleId: createdRoleID,
      });
    }
    if (rootToken && createdRoleID) {
      await callApi(request, 'admin/roles/delete', {
        i: rootToken,
        roleId: createdRoleID,
      });
    }
    createdRoleID = undefined;
    assignedUserID = undefined;
    rootToken = undefined;
  });

  test('admin creates, assigns, unassigns, then deletes a role', async ({
    request,
  }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    rootToken = root.token;
    const target = await signupUser(request, randomUsername('arA'));
    assignedUserID = target.id;

    // create role。upstream Misskey TS は paramDef で多くの field を required
    // にしている (color / iconUrl / target / condFormula / asBadge /
    // canEditMembersByModerator / displayOrder / policies)。mk-go はこれら
    // を optional として受け付けるが、TS と round-trip するには full payload
    // が必要 (#889 で paramDef を揃える方向)。
    const roleName = 'spec_role_' + Math.random().toString(16).slice(2, 8);
    const createResp = await callApi(request, 'admin/roles/create', {
      i: root.token,
      name: roleName,
      description: 'playwright spec role',
      color: null,
      iconUrl: null,
      target: 'manual',
      condFormula: {},
      isPublic: true,
      isModerator: false,
      isAdministrator: false,
      asBadge: false,
      canEditMembersByModerator: false,
      displayOrder: 0,
      policies: {},
    });
    expect(createResp.status()).toBe(200);
    const role = (await createResp.json()) as AdminRole;
    expect(role.id).toBeTruthy();
    expect(role.name).toBe(roleName);
    createdRoleID = role.id;

    // list で含むこと
    const listResp = await callApi(request, 'admin/roles/list', {
      i: root.token,
    });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as AdminRole[];
    expect(list.some((r) => r.id === role.id)).toBe(true);

    // assign
    const assignResp = await callApi(request, 'admin/roles/assign', {
      i: root.token,
      userId: target.id,
      roleId: role.id,
    });
    expect(assignResp.status()).toBe(204);

    // target の /api/i で roles array に role が含まれること
    const meAfterAssign = await callApi(request, 'i', { i: target.token });
    expect(meAfterAssign.status()).toBe(200);
    const meBody = (await meAfterAssign.json()) as { roles: RoleRef[] };
    expect(Array.isArray(meBody.roles)).toBe(true);
    expect(meBody.roles.some((r) => r.id === role.id)).toBe(true);

    // unassign
    const unassign = await callApi(request, 'admin/roles/unassign', {
      i: root.token,
      userId: target.id,
      roleId: role.id,
    });
    expect(unassign.status()).toBe(204);
    // afterEach の re-unassign を skip するために一旦解除済み state にする。
    assignedUserID = undefined;

    // target の /api/i から role が消えること
    const meAfterUnassign = await callApi(request, 'i', { i: target.token });
    expect(meAfterUnassign.status()).toBe(200);
    const meBody2 = (await meAfterUnassign.json()) as { roles: RoleRef[] };
    expect(meBody2.roles.some((r) => r.id === role.id)).toBe(false);

    // delete
    const deleteResp = await callApi(request, 'admin/roles/delete', {
      i: root.token,
      roleId: role.id,
    });
    expect(deleteResp.status()).toBe(204);
    // afterEach の re-delete を skip。
    createdRoleID = undefined;

    // list から消えていること
    const listAfterDelete = await callApi(request, 'admin/roles/list', {
      i: root.token,
    });
    expect(listAfterDelete.status()).toBe(200);
    const listAfter = (await listAfterDelete.json()) as AdminRole[];
    expect(listAfter.some((r) => r.id === role.id)).toBe(false);
  });
});
