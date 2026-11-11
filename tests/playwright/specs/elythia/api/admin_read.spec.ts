/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の独自の API のうち、画面の spec で通っているが応答の型を見ていなかったもの
// (健康状態・連合・IP・サーバーの情報・バックアップの一覧) を叩き、応答を /api.json の
// 宣言で照合する (#4093)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { callChecked, expectApiResponse } from '../../../fixtures/api_schema';
import { rootFixture } from '../../../fixtures/admin_table';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { totpAdmin } from '../../../fixtures/reauth';

// 解決されない予約の名前 (RFC 6761)。ルールの対象にしても、ほかの spec の連合に効かない
const HOST = 'pw-read.invalid';

/**
 * Calls an endpoint whose rows come and go with the other specs, and checks
 * it against the declaration.
 */
async function callWithRows<T = unknown>(request: Parameters<typeof callApi>[0], endpoint: string, body: Record<string, unknown>): Promise<T> {
  const resp = await callApi(request, endpoint, body);
  return expectApiResponse<T>(request, endpoint, resp);
}

test.describe('admin: the Elythia endpoints that read answer as declared', () => {
  test.skip(isTsBackend, 'ここで叩く口は Elythia 独自');

  test.beforeAll(() => {
    resetRateLimit();
  });

  test('the health and the information of the server', async ({ request }) => {
    const { token } = rootFixture();
    await callChecked(request, 'admin/database-health', { i: token });
    await callChecked(request, 'admin/drive/usage', { i: token });
    await callChecked(request, 'admin/server-metrics', { i: token });
    await callChecked(request, 'admin/server-plugins', { i: token });
  });

  // 配送の記録や消えたサーバーの行は、ほかの spec の配送 (リレーの Follow など) で増え減りする。
  // このスタックでは空のことが多く、そのときは外側の型だけを照合している
  test('the health of the federation, and the gone instances', async ({ request }) => {
    const { token } = rootFixture();
    await callWithRows(request, 'admin/federation/delivery-health', { i: token });
    await callWithRows(request, 'admin/federation/inbox-health', { i: token });
    await callWithRows(request, 'admin/federation/gone-instances', { i: token });
  });

  test('list and update a federation rule', async ({ request }) => {
    const { token } = rootFixture();
    const created = await callApi(request, 'admin/federation/rules/create', { i: token, name: 'pw read', mode: 'record', target: 'note', hosts: [HOST], unlist: true });
    expect(created.status(), await created.text()).toBe(200);
    const ruleId = ((await created.json()) as { id: string }).id;
    try {
      const rules = await callChecked<{ id: string }[]>(request, 'admin/federation/rules/list', { i: token });
      expect(rules.map((r) => r.id)).toContain(ruleId);
      const updated = await callChecked<{ id: string; name: string }>(request, 'admin/federation/rules/update', {
        i: token, ruleId, name: 'pw read (updated)', mode: 'record', target: 'note', hosts: [HOST], unlist: true,
      });
      expect(updated).toMatchObject({ id: ruleId, name: 'pw read (updated)' });
    } finally {
      expect((await callApi(request, 'admin/federation/rules/delete', { i: token, ruleId })).status()).toBe(204);
    }
  });

  test('the accounts by IP, the related accounts and the lookup log', async ({ request }) => {
    const { token } = rootFixture();
    const user = await signupUser(request, randomUsername('admip'));
    // 記録は回をまたいで残るので、照会する IP を回ごとに変えて、この回の照会を見分ける (文書用の範囲)
    // 0 にすると正規化で `2001:db8::` になり、記録と一致しなくなるので 1 から
    const ip = `2001:db8::${(1 + Math.floor(Math.random() * 0xfffe)).toString(16)}`;
    await callChecked(request, 'admin/ip/accounts', { i: token, ip });
    await callChecked(request, 'admin/ip/related-accounts', { i: token, userId: user.id });
    // 上の 2 つの照会は記録に残る
    const log = await callChecked<{ entries: { kind: string; ip?: string; targetUserId?: string }[] }>(request, 'admin/ip/lookup-log', { i: token });
    expect(log.entries.some((e) => e.ip === ip), 'the lookup by IP is logged').toBe(true);
    expect(log.entries.some((e) => e.targetUserId === user.id), 'the lookup by user is logged').toBe(true);
  });

  test('the generations of the backups, after re-authenticating with the password and TOTP', async ({ request }) => {
    const root = rootFixture();
    const reauth = await totpAdmin(request, root, 'admbk');
    try {
      // このスタックにはバックアップのデーモンが無く、世代も置かないので、外側の型だけを照合している
      await callWithRows(request, 'admin/backup/list', { i: reauth.admin.token, password: reauth.password, token: reauth.code(1) });
    } finally {
      expect((await callApi(request, 'admin/roles/delete', { i: root.token, roleId: reauth.roleId })).status()).toBe(204);
    }
  });
});
