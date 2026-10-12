/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-E: admin/system-webhook/* full CRUD round-trip。
//
// 6 endpoint: list / create / show / update / test / delete。
// 全 admin 権限要、root token を再利用する。

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

interface SystemWebhook {
  id: string;
  name?: string;
  url?: string;
  secret?: string;
  on?: string[];
  isActive?: boolean;
}

test.describe('admin/system-webhook/* CRUD', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('create → list → show → update → test → delete round-trip', async ({ request }) => {
    const name = `spec_webhook_${randomUUID()}`;

    // 1. create
    const createResp = await callApi(request, 'admin/system-webhook/create', {
      i: root.token,
      isActive: true,
      name,
      on: ['abuseReport'],
      url: 'https://example.invalid/webhook',
      secret: 'spec-secret',
    });
    expect(createResp.status()).toBe(200);
    const created = (await createResp.json()) as SystemWebhook;
    expect(typeof created.id).toBe('string');
    const webhookId = created.id;

    // 2. list で含まれる
    const listResp = await callApi(request, 'admin/system-webhook/list', {
      i: root.token,
    });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as SystemWebhook[];
    expect(Array.isArray(list)).toBe(true);
    expect(list.find((w) => w.id === webhookId)).toBeDefined();

    // 3. show で取得整合性
    const showResp = await callApi(request, 'admin/system-webhook/show', {
      i: root.token,
      id: webhookId,
    });
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as SystemWebhook;
    expect(shown.id).toBe(webhookId);
    expect(shown.name).toBe(name);

    // 4. update で isActive を切替。upstream TS paramDef は
    //    required: ['id','isActive','name','on','url'] なので全 field 必須。
    //    mk-go 側は #932 で pq.StringArray ラップ済 (空 string[] が NULL 化
    //    して NOT NULL 違反になる drift を解消)。
    const updResp = await callApi(request, 'admin/system-webhook/update', {
      i: root.token,
      id: webhookId,
      isActive: false,
      name,
      on: ['abuseReport'],
      url: 'https://example.invalid/webhook-updated',
      secret: 'spec-secret-updated',
    });
    expect(updResp.status()).toBe(200);

    // 5. test (= 仮想 webhook を発火)。実 HTTP 配信は test 環境で fail
    //    するが、handler 側は dispatch を試みて 204 を返す (両 backend)。
    const testResp = await callApi(request, 'admin/system-webhook/test', {
      i: root.token,
      webhookId,
      type: 'abuseReport',
    });
    expect([200, 204]).toContain(testResp.status());

    // 6. delete
    const delResp = await callApi(request, 'admin/system-webhook/delete', {
      i: root.token,
      id: webhookId,
    });
    expect([200, 204]).toContain(delResp.status());

    // 7. list 再取得で消えている
    const listAfter = await callApi(request, 'admin/system-webhook/list', {
      i: root.token,
    });
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as SystemWebhook[];
    expect(listAfterBody.find((w) => w.id === webhookId)).toBeFalsy();
  });
});
