/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の独自の API のうち、前提の要らないもの (診断・連合・ロールの割り当て) を
// 叩き、応答を /api.json の宣言で照合する (#4093)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { callChecked, expectApiResponse } from '../../../fixtures/api_schema';
import { rootFixture } from '../../../fixtures/admin_table';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';

// 解決されない予約の名前 (RFC 6761)。ルールの対象にしても、ほかの spec の連合に効かない
const HOST = 'pw-check.invalid';

interface CheckReport { host?: string; ok: boolean; results: unknown[] }
interface Assignment { assigned: boolean; expiresAt: string | null; role: { id: string } }

test.describe('admin: the Elythia endpoints answer as declared', () => {
  test.skip(isTsBackend, 'ここで叩く口は Elythia 独自');

  test.beforeAll(() => {
    resetRateLimit();
  });

  test('the self check and the check of a remote host', async ({ request }) => {
    // どちらも外へ取りに行く。名前の解決が返らない環境では、取得ごとの上限 (10 秒) まで待つ
    test.setTimeout(90_000);
    const root = rootFixture();
    const self = await callChecked<CheckReport>(request, 'admin/self-check', { i: root.token });
    expect(self.results.length).toBeGreaterThan(0);
    // 名前が解決されないので、診断は失敗の結果を並べて返す
    const remote = await callChecked<CheckReport>(request, 'admin/federation/check-host', { i: root.token, host: HOST });
    expect(remote.host).toBe(HOST);
    expect(remote.ok).toBe(false);
  });

  test('close a delivery breaker, and read the hits of a federation rule', async ({ request }) => {
    const root = rootFixture();
    // 開いていないホストを閉じても 204 で返る (閉じたことはモデレーションのログに残る)
    await callChecked(request, 'admin/federation/close-delivery-breaker', { i: root.token, host: HOST });

    const created = await callApi(request, 'admin/federation/rules/create', {
      i: root.token, name: 'pw hits', mode: 'record', target: 'note', hosts: [HOST], unlist: true,
    });
    expect(created.status(), await created.text()).toBe(200);
    const rule = (await created.json()) as { id: string };
    try {
      // 照合が落ちてもルールを消すよう、照合は作った後で行う。日時は #4091 で直す
      await expectApiResponse(request, 'admin/federation/rules/create', created, { knownBug: '#4091' });
      expect(await callChecked<unknown[]>(request, 'admin/federation/rules/hits', { i: root.token, ruleId: rule.id })).toEqual([]);
    } finally {
      await callChecked(request, 'admin/federation/rules/delete', { i: root.token, ruleId: rule.id });
    }
  });

  test('show the assignment of a role to a user', async ({ request }) => {
    const root = rootFixture();
    const user = await signupUser(request, randomUsername('admra'));
    const created = await callApi(request, 'admin/roles/create', {
      i: root.token, name: `pw-assignment-${Date.now()}`, description: '', color: null, iconUrl: null,
      target: 'manual', condFormula: {}, isPublic: false, isExplorable: false,
      isAdministrator: false, isModerator: false, asBadge: false,
      canEditMembersByModerator: false, displayOrder: 0, policies: {},
    });
    expect(created.status(), await created.text()).toBe(200);
    const roleId = ((await created.json()) as { id: string }).id;
    try {
      const before = await callChecked<Assignment>(request, 'admin/roles/assignment-show', { i: root.token, userId: user.id, roleId });
      expect(before.assigned).toBe(false);
      // 期限付きで割り当て、expiresAt の日時の書式も照合する
      const expiresAt = Date.now() + 24 * 60 * 60 * 1000;
      expect((await callApi(request, 'admin/roles/assign', { i: root.token, roleId, userId: user.id, expiresAt })).status()).toBe(204);
      const after = await callChecked<Assignment>(request, 'admin/roles/assignment-show', { i: root.token, userId: user.id, roleId });
      expect(after).toMatchObject({ assigned: true, role: { id: roleId } });
      expect(Date.parse(after.expiresAt!)).toBe(expiresAt);
    } finally {
      expect((await callApi(request, 'admin/roles/delete', { i: root.token, roleId })).status()).toBe(204);
    }
  });
});
