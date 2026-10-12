/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #817 part1: 2FA (TOTP) signin path。
//
// upstream Misskey TS と mk-go は 2FA 有効ユーザの signin-flow で
// step 1 (= username + password) に対し \`next: 'totp'\` を返し、step 2 で
// \`token\` (TOTP 6 桁) を送って \`finished: true, i: <token>\` を返す
// (#705 で TS 互換化)。
//
// 本 spec は両 backend 共通で:
//   1. signup user → /api/i/2fa/register で TOTP secret 取得
//   2. otplib で TOTP code を生成 → /api/i/2fa/done で 2FA enable
//   3. **次の TOTP step まで待って別 code を生成**
//   4. /api/signin-flow (step 1: username + password) → next: 'totp' assert
//   5. /api/signin-flow (step 2: token=新 code) → finished: true, i: <token> assert
//
// を strict に検証する。
//
// 注: 以前は「1 個の code を done と signin で再利用」していた。根拠は
// 「upstream の TOTP verify は window:5 + replay 拒否なし」だったが、これは
// **現在の両実装で偽**。upstream は 2026.6.0 (#17580) の
// UserAuthService.validateOtp で使用済 code を Redis に SET NX (TTL 90s) して
// 再利用を拒否し、mk-go も同等の replay guard を持つ (RFC 6238 §5.2)。
// そのため done で消費した code を signin に流すと 403 になる。step を跨いで
// 別 code を使うことで、replay guard を回避しつつ本来の signin 経路を検証する。
//
// WebAuthn (passkey) 経路は別 spec (#817 part2 で個別 PR 予定、
// `@simplewebauthn/server` で credential を programmatically 生成する設計)。

import { expect, test } from '@playwright/test';
import { authenticator } from 'otplib';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('auth: 2FA (TOTP)', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  // TOTP step 境界 (最大 30s) を跨いで待つため、default の 30s timeout では
  // 足りない。待機 (最大 32s) + signin rate limit の 1.1s + API 往復で
  // 90s あれば十分な margin になる。
  test('signin-flow returns next:totp for 2FA-enabled user and finishes with TOTP token', async ({ request }) => {
    test.setTimeout(90_000);
    const username = randomUsername('tfa');
    const password = 'password1234';
    const me = await signupUser(request, username, password);

    // 2FA registration: secret 取得。`password` は self confirmation 用。
    const regResp = await callApi(request, 'i/2fa/register', { i: me.token, password });
    expect(regResp.status()).toBe(200);
    const regBody = await regResp.json();
    expect(typeof regBody.secret).toBe('string');
    expect(regBody.secret.length).toBeGreaterThan(0);

    // 2FA enable 用の code。この code は done で「使用済み」として記録される。
    const enableToken = authenticator.generate(regBody.secret);

    // 2FA done: 2FA enable。upstream / mk-go ともに 200 + { backupCodes:
    // [...] } を返す。
    const doneResp = await callApi(request, 'i/2fa/done', { i: me.token, token: enableToken });
    expect(doneResp.status()).toBe(200);
    const doneBody = await doneResp.json();
    expect(Array.isArray(doneBody.backupCodes)).toBe(true);
    expect(doneBody.backupCodes.length).toBeGreaterThan(0);

    // replay guard は (user, secret, step) を key に使用済 code を記録するので、
    // **次の TOTP step に入ってから**新しい code を生成する。timeRemaining() は
    // 現 step の残り秒数。+2s の margin で step 境界を確実に跨ぐ。
    const waitMs = (authenticator.timeRemaining() + 2) * 1000;
    await new Promise((resolve) => setTimeout(resolve, waitMs));
    const signinToken = authenticator.generate(regBody.secret);
    expect(signinToken).not.toBe(enableToken);

    // signin-flow step 1: username + password → next: 'totp'。
    const step1Resp = await callApi(request, 'signin-flow', { username, password });
    expect(step1Resp.status()).toBe(200);
    const step1Body = await step1Resp.json();
    expect(step1Body.finished).toBe(false);
    expect(step1Body.next).toBe('totp');

    // signin-flow step 2: TOTP token を送信 → finished: true。
    // upstream Misskey TS は signin rate limit に minInterval: 1000ms が
    // ある (SigninApiService.ts) ので、step 1 と step 2 を 1 秒以上
    // 空けて呼ぶ必要がある。1.1s 待機で margin。
    await new Promise((resolve) => setTimeout(resolve, 1100));
    const step2Resp = await callApi(request, 'signin-flow', { username, password, token: signinToken });
    expect(step2Resp.status()).toBe(200);
    const step2Body = await step2Resp.json();
    expect(step2Body.finished).toBe(true);
    expect(typeof step2Body.i).toBe('string');
    expect(step2Body.i.length).toBeGreaterThan(0);
  });
});
