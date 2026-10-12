/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-C: admin/abuse-report/* + admin/abuse-user-reports spec。
//
// 6 endpoint:
//   - admin/abuse-user-reports: 配列 shape
//   - admin/abuse-report/notification-recipient: create / list / show / update / delete の 1 round-trip
//
// すべて moderator 権限要、root token を再利用する。

import { randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

interface Recipient {
  id: string;
  name?: string;
  method?: string;
  isActive?: boolean;
}

test.describe('admin/abuse-* shape compat', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('admin/abuse-user-reports returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'admin/abuse-user-reports', {
      i: root.token,
      limit: 5,
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('notification-recipient: create → list → show → update → delete round-trip', async ({
    request,
  }) => {
    // TS は method='email' で root の email 未設定の場合 EMAIL_ADDRESS_NOT_SET
    // (400) を返す。mk-go は同 check 無し。両 backend で動かすため [200, 400]
    // LCD し、400 のとき後続 step を skip する設計 (drift 詳細は別 issue 候補)。
    const name = `spec_recipient_${randomUUID()}`;

    // 1. create
    // upstream Misskey TS は paramDef で isActive / name / method を required
    // にしている。method='email' の場合さらに userId 必須 (相関 check)。
    // root user の id を userId として渡す。
    const createResp = await callApi(
      request,
      'admin/abuse-report/notification-recipient/create',
      {
        i: root.token,
        name,
        method: 'email',
        isActive: true,
        userId: root.id,
      },
    );
    expect([200, 400]).toContain(createResp.status());
    if (createResp.status() === 400) {
      // TS で root に email 未設定の場合、後続 step は skip。
      // mk-go では 200 になるので round-trip が完走する。
      return;
    }
    const created = (await createResp.json()) as Recipient;
    expect(typeof created.id).toBe('string');
    expect(created.name).toBe(name);
    const recipientId = created.id;

    // 2. list で含まれる
    const listResp = await callApi(
      request,
      'admin/abuse-report/notification-recipient/list',
      { i: root.token },
    );
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as Recipient[];
    expect(Array.isArray(list)).toBe(true);
    expect(list.find((r) => r.id === recipientId)).toBeDefined();

    // 3. show で取得整合性
    const showResp = await callApi(
      request,
      'admin/abuse-report/notification-recipient/show',
      { i: root.token, id: recipientId },
    );
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as Recipient;
    expect(shown.id).toBe(recipientId);
    expect(shown.name).toBe(name);

    // 4. update で isActive 切替
    const updResp = await callApi(
      request,
      'admin/abuse-report/notification-recipient/update',
      { i: root.token, id: recipientId, isActive: false },
    );
    expect(updResp.status()).toBe(200);
    const updated = (await updResp.json()) as Recipient;
    expect(updated.isActive).toBe(false);

    // 5. delete → 204 / list 再取得で消えている
    const delResp = await callApi(
      request,
      'admin/abuse-report/notification-recipient/delete',
      { i: root.token, id: recipientId },
    );
    expect([200, 204]).toContain(delResp.status());

    const listAfter = await callApi(
      request,
      'admin/abuse-report/notification-recipient/list',
      { i: root.token },
    );
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as Recipient[];
    expect(listAfterBody.find((r) => r.id === recipientId)).toBeFalsy();
  });
});
