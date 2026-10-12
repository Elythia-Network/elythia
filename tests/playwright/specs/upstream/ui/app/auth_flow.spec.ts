/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #834: app/* + auth/session/* の 3rd party app auth flow。
//
// upstream Misskey TS と mk-go は両方とも:
//   1. app/create でサードパーティ app を登録、appSecret を取得
//   2. app/show で同 app が引ける
//   3. auth/session/generate { appSecret } で session token を取得
//   4. auth/session/show { token } で session 情報を確認 (pending state)
//   5. user が auth/accept { token } で承認 (要 user token)
//   6. auth/session/userkey { appSecret, token } で access token を取得
//   7. その access token で /api/i が叩ける = 認証完成
//
// upstream Misskey の app token (= 3rd party 用) と /signin-flow の i-token
// (= ユーザー直接 login) を区別する設計の round-trip 全体を 1 spec で検証。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface App {
  id: string;
  name: string;
  secret?: string;
}

test.describe('app + auth/session 3rd party flow', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('app/create → auth/session/generate → accept → userkey → /api/i works', async ({
    request,
  }) => {
    const me = await signupUser(request, randomUsername('app'));
    const appName = `spec_app_${Math.random().toString(16).slice(2, 8)}`;

    // 1. app create
    const createResp = await callApi(request, 'app/create', {
      i: me.token,
      name: appName,
      description: 'spec test app',
      permission: ['read:account'],
      callbackUrl: 'https://example.com/callback',
    });
    expect(createResp.status()).toBe(200);
    const app = (await createResp.json()) as App;
    expect(typeof app.id).toBe('string');
    expect(app.name).toBe(appName);
    expect(typeof app.secret).toBe('string');
    const secret = app.secret!;

    // 2. app/show (= 同 id で取得して shape 一致)
    const showResp = await callApi(request, 'app/show', {
      i: me.token,
      appId: app.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as App;
    expect(shown.id).toBe(app.id);
    expect(shown.name).toBe(appName);

    // 3. auth/session/generate { appSecret }
    const sessionResp = await callApi(request, 'auth/session/generate', {
      appSecret: secret,
    });
    expect(sessionResp.status()).toBe(200);
    const session = (await sessionResp.json()) as { token: string; url: string };
    expect(typeof session.token).toBe('string');
    // url は user が踏む承認 URL (= /auth/<token>)。frontend test では
    // Playwright の page.goto で承認 UI を踏む経路を取るが、本 spec は
    // backend API レイヤのみなので存在のみ確認 (= drop-in compat としては
    // 文字列 URL が返れば OK)。
    expect(typeof session.url).toBe('string');

    // 4. auth/session/show で pending 状態
    const sessionShowResp = await callApi(request, 'auth/session/show', {
      token: session.token,
    });
    expect(sessionShowResp.status()).toBe(200);
    // session.user は accept 前は null

    // 5. user が auth/accept で承認 (実 frontend では UI から)
    const acceptResp = await callApi(request, 'auth/accept', {
      i: me.token,
      token: session.token,
    });
    expect([200, 204]).toContain(acceptResp.status());

    // 6. auth/session/userkey で access token を受取り
    const userkeyResp = await callApi(request, 'auth/session/userkey', {
      appSecret: secret,
      token: session.token,
    });
    expect(userkeyResp.status()).toBe(200);
    const userkey = (await userkeyResp.json()) as {
      accessToken?: string;
      user?: { id: string };
    };
    // accessToken / user は upstream / mk-go 共通の shape で返る
    expect(typeof userkey.accessToken).toBe('string');
    expect(userkey.accessToken!.length).toBeGreaterThan(0);
    expect(userkey.user?.id).toBe(me.id);
    // userkey が me.token (= signup で取得した session token) と等しくない
    // = app 経由で新規 access token が発行されている。session 流用の regression
    // を防ぐ。
    expect(userkey.accessToken).not.toBe(me.token);

    // 7. 取得 token で /api/i が叩ける = 認証完了。
    // mk-go では auth/accept で access_token を sha256(token + app.secret) で
    // store するため、middleware は hash 列 OR token (raw) 列の dual lookup
    // で resolve する (#910)。upstream Misskey TS と同じ pattern。
    const iResp = await callApi(request, 'i', { i: userkey.accessToken });
    expect(iResp.status()).toBe(200);
    const iBody = (await iResp.json()) as { id: string };
    expect(iBody.id).toBe(me.id);
  });
});
