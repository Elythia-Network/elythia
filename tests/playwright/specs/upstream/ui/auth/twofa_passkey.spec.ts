/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #817 part2: 2FA (WebAuthn passkey) signin path。
//
// upstream Misskey TS と mk-go は 2FA + security key 登録済ユーザの
// signin-flow で step 1 (= username + password) に対し
// `next: 'passkey'` + `authRequest` (PublicKeyCredentialRequestOptionsJSON)
// を返し、step 2 で `credential` を送って `finished: true, i: <token>`
// を返す (#705 で TS 互換化、cypress webauthn.cy.ts も同 pattern を cover)。
//
// 本 spec は両 backend 共通で:
//   1. signup user → 2FA (TOTP) を enable (= security key 登録の前提条件)
//   2. /api/i/2fa/register-key で WebAuthn challenge (creationOptions) を取得
//   3. Browser + CDP Virtual Authenticator で credential を作成
//   4. /api/i/2fa/key-done で credential を送信、key 登録完了
//   5. /api/signin-flow step 1 (username + password) →
//      next: 'passkey' + authRequest assert
//   6. Browser で navigator.credentials.get で assertion を取得
//   7. /api/signin-flow step 2 (with credential) →
//      finished: true, i: <token> assert
//
// を strict に検証する。
//
// 設計判断: 既存 Phase 1 spec は API-only だが、本 spec は WebAuthn の
// 特殊性 (= attestation/assertion を programmatically 生成するには
// browser API or 重い emulator が必要) のため、Browser + Chrome DevTools
// Protocol の Virtual Authenticator を使う唯一の exception とする。
// Playwright runner image は browser 内蔵で追加コストなし。
// cypress webauthn.cy.ts と同じ pattern の Playwright 翻訳。

import { expect, test } from '@playwright/test';
import { authenticator } from 'otplib';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

// 本 spec は CDP Virtual Authenticator の receptacle として page fixture を
// 使うが、navigate 先が /api/ping (= JSON endpoint) で実 UI は表示されない
// (= 動画は真っ白)。video が debug 価値ゼロなので本 spec のみ off にする。
// trace / screenshot の retain-on-failure は維持。
//
// 注: `test.use({ video: ... })` は describe 内に置くと
// "Cannot use({ video }) in a describe group, because it forces a new worker"
// で reject されるので必ず file top-level に置く。
test.use({ video: 'off' });

test.describe('auth: 2FA (WebAuthn passkey)', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('signin-flow returns next:passkey for security-key-enabled user and finishes with credential', async ({ page, request }) => {
    // browser の page は CDP Virtual Authenticator を使うために必要。
    // `about:blank` は origin が null で navigator.credentials が undefined
    // になるため、実 instance origin (= MK_BASE_URL = https://mkgo.local)
    // を navigate して secure context を確立する。`/api/ping` は軽量
    // endpoint で 200 + JSON を返し、frontend SPA を load せず origin
    // context だけ確立できる。
    const baseURL = process.env.MK_BASE_URL ?? 'https://mkgo.local';
    await page.goto(`${baseURL}/api/ping`, { waitUntil: 'commit' });

    // CDP Virtual Authenticator 有効化。ctap2 + resident key + UV verified
    // で実 USB key を再現する。spec 終了時に page が close されると
    // authenticator も破棄される。
    const cdp = await page.context().newCDPSession(page);
    await cdp.send('WebAuthn.enable');
    await cdp.send('WebAuthn.addVirtualAuthenticator', {
      options: {
        protocol: 'ctap2',
        transport: 'usb',
        hasResidentKey: true,
        hasUserVerification: true,
        isUserVerified: true,
      },
    });

    // signup + 2FA (TOTP) enable。security key 登録は 2FA enabled が前提。
    const username = randomUsername('pkey');
    const password = 'password1234';
    const me = await signupUser(request, username, password);

    const regResp = await callApi(request, 'i/2fa/register', { i: me.token, password });
    expect(regResp.status()).toBe(200);
    const regBody = await regResp.json();
    const totpToken = authenticator.generate(regBody.secret);
    const doneResp = await callApi(request, 'i/2fa/done', { i: me.token, token: totpToken });
    expect(doneResp.status()).toBe(200);

    // mk-go は TOTP の replay guard (RFC 6238 §5.2) を持つ独自 hardening が
    // あり、i/2fa/done で消費した code を register-key / key-done で再利用
    // すると 403 になる。同一 time step 内では新しい code も作れないので、
    // 以降の 2FA verify には done が返す **single-use backup code** を
    // 1 個ずつ使う (verify2FAToken は backup code も受け付ける)。
    const backupCodes: string[] = (await doneResp.json()).backupCodes;
    expect(Array.isArray(backupCodes)).toBe(true);
    expect(backupCodes.length).toBeGreaterThanOrEqual(2);

    // /api/i/2fa/register-key で WebAuthn creationOptions を取得。
    // upstream は #705 fix 後 PublicKeyCredentialCreationOptions を直返し
    // (publicKey wrapper / sessionId なし)。
    const regKeyResp = await callApi(request, 'i/2fa/register-key', {
      i: me.token,
      password,
      token: backupCodes[0],
    });
    expect(regKeyResp.status()).toBe(200);
    const creationOpts = await regKeyResp.json();
    expect(creationOpts).toHaveProperty('challenge');
    expect(creationOpts).toHaveProperty('rp');

    // Browser で navigator.credentials.create を呼んで credential を作成。
    // base64url ↔ Uint8Array 変換は page.evaluate 内で手書き (= browser
    // context にも util を import せず closure で完結)。
    const createdCred = await page.evaluate(async (opts) => {
      const b64uDecode = (s: string) =>
        Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/')), (c) => c.charCodeAt(0));
      const b64uEncode = (buf: ArrayBuffer) =>
        btoa(String.fromCharCode(...new Uint8Array(buf)))
          .replace(/\+/g, '-')
          .replace(/\//g, '_')
          .replace(/=+$/, '');
      const publicKey: PublicKeyCredentialCreationOptions = {
        ...(opts as any),
        challenge: b64uDecode((opts as any).challenge),
        user: { ...(opts as any).user, id: b64uDecode((opts as any).user.id) },
        excludeCredentials: ((opts as any).excludeCredentials ?? []).map((c: any) => ({
          ...c,
          id: b64uDecode(c.id),
        })),
      };
      const cred = (await navigator.credentials.create({ publicKey })) as PublicKeyCredential;
      const att = cred.response as AuthenticatorAttestationResponse;
      return {
        id: cred.id,
        rawId: b64uEncode(cred.rawId),
        type: cred.type,
        response: {
          clientDataJSON: b64uEncode(att.clientDataJSON),
          attestationObject: b64uEncode(att.attestationObject),
        },
      };
    }, creationOpts);

    // /api/i/2fa/key-done で credential 登録。
    const keyDoneResp = await callApi(request, 'i/2fa/key-done', {
      i: me.token,
      password,
      token: backupCodes[1],
      name: 'virtual-key',
      credential: createdCred,
    });
    expect(keyDoneResp.status()).toBe(200);

    // signin rate limit (minInterval 1000ms) を避けるため 1.1s 待機。
    await new Promise((resolve) => setTimeout(resolve, 1100));

    // signin-flow step 1: 2FA + key 登録済 → next: 'passkey' + authRequest。
    const step1Resp = await callApi(request, 'signin-flow', { username, password });
    expect(step1Resp.status()).toBe(200);
    const step1Body = await step1Resp.json();
    expect(step1Body.finished).toBe(false);
    expect(step1Body.next).toBe('passkey');
    expect(step1Body).toHaveProperty('authRequest');
    const authRequest = step1Body.authRequest;
    expect(typeof authRequest.challenge).toBe('string');

    // Browser で navigator.credentials.get を呼んで assertion を取得。
    const assertionCred = await page.evaluate(async (req) => {
      const b64uDecode = (s: string) =>
        Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/')), (c) => c.charCodeAt(0));
      const b64uEncode = (buf: ArrayBuffer) =>
        btoa(String.fromCharCode(...new Uint8Array(buf)))
          .replace(/\+/g, '-')
          .replace(/\//g, '_')
          .replace(/=+$/, '');
      const publicKey: PublicKeyCredentialRequestOptions = {
        challenge: b64uDecode((req as any).challenge),
        rpId: (req as any).rpId,
        allowCredentials: ((req as any).allowCredentials ?? []).map((c: any) => ({
          ...c,
          id: b64uDecode(c.id),
        })),
        timeout: (req as any).timeout,
        userVerification: (req as any).userVerification,
      };
      const cred = (await navigator.credentials.get({ publicKey })) as PublicKeyCredential;
      const ass = cred.response as AuthenticatorAssertionResponse;
      return {
        id: cred.id,
        rawId: b64uEncode(cred.rawId),
        type: cred.type,
        response: {
          authenticatorData: b64uEncode(ass.authenticatorData),
          clientDataJSON: b64uEncode(ass.clientDataJSON),
          signature: b64uEncode(ass.signature),
          userHandle: ass.userHandle ? b64uEncode(ass.userHandle) : null,
        },
      };
    }, authRequest);

    // signin rate limit (minInterval 1000ms) 回避。
    await new Promise((resolve) => setTimeout(resolve, 1100));

    // signin-flow step 2: credential 送信 → finished: true。
    const step2Resp = await callApi(request, 'signin-flow', {
      username,
      password,
      credential: assertionCred,
    });
    expect(step2Resp.status()).toBe(200);
    const step2Body = await step2Resp.json();
    expect(step2Body.finished).toBe(true);
    expect(typeof step2Body.i).toBe('string');
    expect(step2Body.i.length).toBeGreaterThan(0);
  });
});
