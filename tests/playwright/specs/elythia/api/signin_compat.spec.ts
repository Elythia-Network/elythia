/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 旧い path の `signin` (Elythia の互換の口。本家は signin-flow だけ) を叩く (#4093)。
// endpoint の外で受ける口なので /api.json に載らず、宣言の型では照合できない。
// 1 段目は 2FA の有無に関わらず password を求める (signin-flow は 2FA が無いと captcha)。
// 2 段目の応答は signin-flow と同じ形なので、その形と、返ったトークンが使えることを確かめる。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';

test.describe('signin: the old path still signs in', () => {
  test.skip(isTsBackend, 'signin は Elythia の互換の口');

  test.beforeAll(() => {
    resetRateLimit();
  });

  test('asks for the password, then answers with a token that works', async ({ request }) => {
    const user = await signupUser(request, randomUsername('sgin'));

    const first = await callApi(request, 'signin', { username: user.username });
    expect(first.status(), await first.text()).toBe(200);
    expect(await first.json()).toEqual({ finished: false, next: 'password' });

    const done = await callApi(request, 'signin', { username: user.username, password: DEFAULT_TEST_PASSWORD });
    expect(done.status(), await done.text()).toBe(200);
    const body = (await done.json()) as { finished: boolean; id: string; i: string };
    expect(body).toEqual({ finished: true, id: user.id, i: expect.any(String) });
    const me = await callApi(request, 'i', { i: body.i });
    expect(me.status()).toBe(200);
    expect(((await me.json()) as { id: string }).id).toBe(user.id);

    // 同じ 403 は凍結でも返るので、パスワードの誤りの id まで見る
    const wrong = await callApi(request, 'signin', { username: user.username, password: 'wrong-password' });
    expect(wrong.status()).toBe(403);
    expect(((await wrong.json()) as { error: { id: string } }).error.id).toBe('932c904e-9460-45b7-9ce6-7ed33be7eb2c');
  });
});
