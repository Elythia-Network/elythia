/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #834: invite/* round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - invite/create で invitation ticket を発行 (=  registration_ticket row)
//   - invite/list で自分が発行した ticket 一覧
//   - invite/delete で ticket を削除
//   - invite/limit で残発行可能数を返す
//
// upstream Misskey TS の invite/create には `requiredRolePolicy: 'canInvite'`
// が指定されており、デフォルトでは一般 user は 403 (= role policy 未付与)。
// root user は全 policy を満たすので spec では root token で走らせる。
// mk-go は role policy 判定を行うが root には常に許可を出すので互換。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

interface InviteTicket {
  id: string;
  code: string;
  used?: boolean;
}

test.describe('invite/* CRUD round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('create / list / delete / limit round-trip', async ({ request }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));

    // create
    const createResp = await callApi(request, 'invite/create', { i: root.token });
    expect(createResp.status()).toBe(200);
    const created = (await createResp.json()) as InviteTicket;
    expect(typeof created.id).toBe('string');
    expect(typeof created.code).toBe('string');
    expect(created.code.length).toBeGreaterThan(0);
    expect(created.used).toBe(false);

    // list で自分の ticket が含まれる
    const listResp = await callApi(request, 'invite/list', { i: root.token });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as InviteTicket[];
    expect(Array.isArray(list)).toBe(true);
    expect(list.find((t) => t.id === created.id)).toBeDefined();

    // limit shape (= remaining counter)
    const limitResp = await callApi(request, 'invite/limit', { i: root.token });
    expect(limitResp.status()).toBe(200);
    const limitBody = (await limitResp.json()) as { remaining?: unknown };
    // upstream paramDef は `remaining: nullable: true` で、TS は inviteLimit policy
    // が無い user (root を含む) で null を返す。mk-go は numeric を返すケースが
    // あるので number | null を許容する LCD で固定する。
    const remaining = limitBody.remaining;
    expect(
      remaining === null || typeof remaining === 'number',
      `remaining should be number | null, got ${typeof remaining}: ${JSON.stringify(remaining)}`,
    ).toBe(true);

    // delete
    const deleteResp = await callApi(request, 'invite/delete', {
      i: root.token,
      inviteId: created.id,
    });
    expect([200, 204]).toContain(deleteResp.status());

    // delete 後 list から消える
    const listAfter = await callApi(request, 'invite/list', { i: root.token });
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as InviteTicket[];
    expect(listAfterBody.find((t) => t.id === created.id)).toBeFalsy();
  });
});
