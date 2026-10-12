/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-F: i/webhooks/* full CRUD round-trip。
//
// 6 endpoint: list / create / show / update / test / delete (= user-level
// webhook、admin/system-webhook の user 版)。

import { randomUUID } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface UserWebhook {
  id: string;
  name?: string;
  url?: string;
  on?: string[];
  isActive?: boolean;
}

test.describe('i/webhooks/* CRUD', () => {
  let userToken: string;

  test.beforeAll(async ({ request }) => {
    resetRateLimit();
    const me = await signupUser(request, randomUsername('wh'));
    userToken = me.token;
  });

  test('create → list → show → update → test → delete round-trip', async ({ request }) => {
    const name = `spec_user_webhook_${randomUUID()}`;

    // 1. create
    const createResp = await callApi(request, 'i/webhooks/create', {
      i: userToken,
      name,
      url: 'https://example.invalid/user-webhook',
      secret: 'spec-secret',
      on: ['mention'],
    });
    expect(createResp.status()).toBe(200);
    const created = (await createResp.json()) as UserWebhook;
    expect(typeof created.id).toBe('string');
    const webhookId = created.id;

    // 2. list で含まれる
    const listResp = await callApi(request, 'i/webhooks/list', { i: userToken });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as UserWebhook[];
    expect(Array.isArray(list)).toBe(true);
    expect(list.find((w) => w.id === webhookId)).toBeDefined();

    // 3. show
    const showResp = await callApi(request, 'i/webhooks/show', {
      i: userToken,
      webhookId,
    });
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as UserWebhook;
    expect(shown.id).toBe(webhookId);

    // 4. update。upstream TS は body 無しの 204 を返す (#936 fix 後は mk-go も
    //    204 統一)。caller が更新後 row を必要なら i/webhooks/show で取り直す。
    const updResp = await callApi(request, 'i/webhooks/update', {
      i: userToken,
      webhookId,
      name,
      url: 'https://example.invalid/user-webhook-updated',
      secret: 'spec-secret-updated',
      on: ['mention'],
      isActive: false,
    });
    expect(updResp.status()).toBe(204);

    // 5. test (= 仮想 webhook を発火)。upstream paramDef は webhookId + type 必須 +
    //    type に webhookEventTypes enum 制約 (#937 fix 後は mk-go も同 strictness)。
    const testResp = await callApi(request, 'i/webhooks/test', {
      i: userToken,
      webhookId,
      type: 'mention',
    });
    expect(testResp.status()).toBe(204);

    // 6. delete
    const delResp = await callApi(request, 'i/webhooks/delete', {
      i: userToken,
      webhookId,
    });
    expect([200, 204]).toContain(delResp.status());

    // 7. list 再取得で消えている
    const listAfter = await callApi(request, 'i/webhooks/list', { i: userToken });
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as UserWebhook[];
    expect(listAfterBody.find((w) => w.id === webhookId)).toBeFalsy();
  });
});
