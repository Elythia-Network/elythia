/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #832: sw/* (service worker push subscription) round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - sw/register: { endpoint, auth, publickey, sendReadMessage } で
//     subscription を作成 → { state: 'subscribed', userId, endpoint, ... }
//   - sw/show-registration: { endpoint } で取得 → 該当 row なしなら null
//   - sw/update-registration: sendReadMessage を切替
//   - sw/unregister: { endpoint, auth, publickey } (2026.10.0 から 3 つとも必須)
//     が一致する行を削除 (credential 不要)
//
// 実 push 配信は browser-side / push server 必要なので scope 外。本 spec は
// CRUD round-trip の shape 整合のみ verify する。

import { randomUUID } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('sw/* push subscription round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('register / show / update / unregister round-trip', async ({ request }) => {
    const me = await signupUser(request, randomUsername('sw'));
    // dummy push endpoint + key (= 実 VAPID 検証は backend で行わないので、
    // 構造化された non-empty string を入れれば shape 互換 verify には十分)。
    // worker 並列化時の endpoint 衝突を避けるため UUID v4 で path を組む。
    const endpoint = `https://push.example.test/${randomUUID()}`;
    const auth = 'spec-auth-key';
    const publickey = 'spec-publickey-base64url';

    // 1. register
    const regResp = await callApi(request, 'sw/register', {
      i: me.token,
      endpoint,
      auth,
      publickey,
      sendReadMessage: false,
    });
    expect(regResp.status()).toBe(200);
    const regBody = (await regResp.json()) as {
      state?: string;
      userId?: string;
      endpoint?: string;
      sendReadMessage?: boolean;
    };
    expect(regBody.state).toBe('subscribed');
    expect(regBody.userId).toBe(me.id);
    expect(regBody.endpoint).toBe(endpoint);
    expect(regBody.sendReadMessage).toBe(false);

    // 2. show-registration: 同 endpoint で取得
    const showResp = await callApi(request, 'sw/show-registration', {
      i: me.token,
      endpoint,
    });
    expect(showResp.status()).toBe(200);
    const showBody = (await showResp.json()) as {
      userId?: string;
      endpoint?: string;
      sendReadMessage?: boolean;
    };
    expect(showBody.userId).toBe(me.id);
    expect(showBody.endpoint).toBe(endpoint);
    expect(showBody.sendReadMessage).toBe(false);

    // 3. update-registration: sendReadMessage を true に
    const updResp = await callApi(request, 'sw/update-registration', {
      i: me.token,
      endpoint,
      sendReadMessage: true,
    });
    expect(updResp.status()).toBe(200);
    const updBody = (await updResp.json()) as {
      userId?: string;
      endpoint?: string;
      sendReadMessage?: boolean;
    };
    // upstream / mk-go 共通で全 3 field 返るので shape 互換を register / show と
    // 同じ強度で verify する。
    expect(updBody.userId).toBe(me.id);
    expect(updBody.endpoint).toBe(endpoint);
    expect(updBody.sendReadMessage).toBe(true);

    // 4. show 再取得で update 反映確認
    const showAfter = await callApi(request, 'sw/show-registration', {
      i: me.token,
      endpoint,
    });
    expect(showAfter.status()).toBe(200);
    const showAfterBody = (await showAfter.json()) as { sendReadMessage?: boolean };
    expect(showAfterBody.sendReadMessage).toBe(true);

    // 5. unregister: credential 不要 (= browser 側 deactivate でも叩ける)。
    // 2026.10.0 から auth / publickey も必須で、endpoint だけでは 400 になる。
    const unregMissing = await callApi(request, 'sw/unregister', {
      endpoint,
    });
    expect(unregMissing.status()).toBe(400);
    const unregResp = await callApi(request, 'sw/unregister', {
      endpoint,
      auth,
      publickey,
    });
    expect([200, 204]).toContain(unregResp.status());

    // 6. unregister 後の show は 204 No Content (= 該当 row なし)。
    // upstream Misskey TS / mk-go (#918 fix 済) ともに同 shape。
    const showFinal = await callApi(request, 'sw/show-registration', {
      i: me.token,
      endpoint,
    });
    expect(showFinal.status()).toBe(204);
  });

  test('register same endpoint twice returns already-subscribed', async ({ request }) => {
    const me = await signupUser(request, randomUsername('sw2'));
    const endpoint = `https://push.example.test/${randomUUID()}`;
    const params = {
      i: me.token,
      endpoint,
      auth: 'spec-auth-key',
      publickey: 'spec-publickey-base64url',
      sendReadMessage: false,
    };

    // 初回 register
    const first = await callApi(request, 'sw/register', params);
    expect(first.status()).toBe(200);
    expect(((await first.json()) as { state?: string }).state).toBe('subscribed');

    // 同 endpoint で再 register → already-subscribed
    const second = await callApi(request, 'sw/register', params);
    expect(second.status()).toBe(200);
    expect(((await second.json()) as { state?: string }).state).toBe('already-subscribed');
  });
});
