/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #834: miauth/gen-token round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - miauth/gen-token { permission, name?, description?, iconUrl? } で
//     直接 access_token を発行する (= 3rd party app 経由ではなく user 自身が
//     簡易 token を作る MiAuth flow)
//   - 返ってきた token で /api/i が通る = 認証完成
//
// app/auth/session 経路と異なり app 登録不要で軽量。frontend が config 画面で
// API token を直接生成する pattern にも使われる。

import { randomUUID } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('miauth/gen-token round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('gen-token returns access token usable on /api/i', async ({ request }) => {
    const me = await signupUser(request, randomUsername('miauth'));

    // upstream Misskey TS は paramDef で `session` を required: ['session', 'permission']
    // としており null でも OK だが key 自体が無いと 400 を返す。mk-go は session を
    // optional 扱いだが、shape 互換のために常に session を送る。frontend miauth
    // 画面が UUID v4 を生成して付ける挙動と厳密に一致させる。
    const session = randomUUID();
    const genResp = await callApi(request, 'miauth/gen-token', {
      i: me.token,
      session,
      name: 'spec-miauth',
      description: 'phase3 miauth spec',
      permission: ['read:account'],
    });
    expect(genResp.status()).toBe(200);
    const genBody = (await genResp.json()) as { token?: string };
    expect(typeof genBody.token).toBe('string');
    expect(genBody.token!.length).toBeGreaterThan(0);
    // me.token (signup で取得した session token) とは別の新規 access token が
    // 発行されているはず。miauth が単に既存 session を返すだけの実装になって
    // いた場合に detect できる。
    expect(genBody.token).not.toBe(me.token);

    // 取得 token で /api/i が叩ける = 認証完成
    const iResp = await callApi(request, 'i', { i: genBody.token });
    expect(iResp.status()).toBe(200);
    const iBody = (await iResp.json()) as { id: string };
    expect(iBody.id).toBe(me.id);
  });
});
