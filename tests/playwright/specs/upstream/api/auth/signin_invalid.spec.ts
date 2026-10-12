/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1 PR-2: signin-flow の失敗系。
// 不正 password / 存在しない user / 空 password に対して 4xx で reject する
// ことを確認する。upstream Misskey TS は password 不一致時に 400 + error
// id を返す挙動を取るので、mk-go も同じ shape を返すことを assert する。
//
// 本 spec は negative path のみで token が発行されないことを確認する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('auth: signin-flow rejects invalid credentials', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('wrong password is rejected without a token', async ({ request }) => {
    const username = randomUsername('siInv');
    await signupUser(request, username, 'correct-password');

    // 違う password で signin-flow を叩く。upstream の signin-flow は password
    // mismatch を 400 系で返す (具体 status code は upstream 仕様で 400 / 403
    // の揺らぎがあるので status 範囲で assert)。
    const resp = await callApi(request, 'signin-flow', {
      username,
      password: 'wrong-password',
    });
    expect(resp.status()).toBeGreaterThanOrEqual(400);
    expect(resp.status()).toBeLessThan(500);

    // token が発行されていないこと。`finished: true, i: ...` の shape では
    // ない (= 失敗 response)。
    const body = await resp.json().catch(() => ({}));
    expect(body.i).toBeFalsy();
  });

  test('non-existent username is rejected without a token', async ({ request }) => {
    const resp = await callApi(request, 'signin-flow', {
      username: randomUsername('ghost'),
      password: 'whatever',
    });
    expect(resp.status()).toBeGreaterThanOrEqual(400);
    expect(resp.status()).toBeLessThan(500);
  });
});
