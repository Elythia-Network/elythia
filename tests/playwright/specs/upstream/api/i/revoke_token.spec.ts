/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #913: i/revoke-token の round-trip。app/auth flow で発行された access token も
// raw token 文字列で revoke できることを確認する drop-in compat regression
// guard。
//
// upstream Misskey TS と mk-go は両方とも:
//   1. app/create + auth/session/generate + accept + userkey で access token を取得
//   2. /api/i/revoke-token { token: <raw> } で raw token 経由で失効
//   3. revoke 後にその token で /api/i を叩くと 401
//
// mk-go は #913 fix 前は raw token を sha256 して hash 列だけで lookup して
// いたため、auth/accept (= hash = sha256(token + app.secret)) で発行した
// token は raw 経由で revoke できなかった drift がある。fix 後は middleware
// と同じ FindByHashOrToken で hash / token 両列を OR 検索する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('i/revoke-token raw-token round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('revoke app-issued access token by raw token', async ({ request }) => {
    const me = await signupUser(request, randomUsername('rvk'));
    const appName = `spec_rvk_${Math.random().toString(16).slice(2, 8)}`;

    // app + auth/session で access token を取得 (= #834 spec と同じ flow)
    const createResp = await callApi(request, 'app/create', {
      i: me.token,
      name: appName,
      description: 'spec revoke',
      permission: ['read:account'],
      callbackUrl: 'https://example.com/cb',
    });
    expect(createResp.status()).toBe(200);
    const { secret } = (await createResp.json()) as { secret: string };

    const sessionResp = await callApi(request, 'auth/session/generate', {
      appSecret: secret,
    });
    expect(sessionResp.status()).toBe(200);
    const { token: sessionToken } = (await sessionResp.json()) as { token: string };

    const acceptResp = await callApi(request, 'auth/accept', {
      i: me.token,
      token: sessionToken,
    });
    expect([200, 204]).toContain(acceptResp.status());

    const userkeyResp = await callApi(request, 'auth/session/userkey', {
      appSecret: secret,
      token: sessionToken,
    });
    expect(userkeyResp.status()).toBe(200);
    const { accessToken } = (await userkeyResp.json()) as { accessToken: string };
    expect(typeof accessToken).toBe('string');

    // 取得 token で /api/i が叩ける = 認証成功
    const beforeRevoke = await callApi(request, 'i', { i: accessToken });
    expect(beforeRevoke.status()).toBe(200);

    // raw token 文字列で revoke (= drift があった経路)
    const revokeResp = await callApi(request, 'i/revoke-token', {
      i: me.token,
      token: accessToken,
    });
    expect([200, 204]).toContain(revokeResp.status());

    // revoke 後は 401 = 失効が反映されている
    const afterRevoke = await callApi(request, 'i', { i: accessToken });
    expect(afterRevoke.status()).toBe(401);
  });
});
