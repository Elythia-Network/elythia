/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1 PR-2: signup-flow で複数 user を作成できることを確認する。
// admin/accounts/create と違って /api/signup は 2 人目以降も通る (= captcha /
// invitation などの制限がない default config を前提)。
//
// 本 spec は drop-in 互換の重要 path: mk-go が upstream Misskey TS と同じ
// signup endpoint を提供し、token / id を期待 shape で返すことを assert
// する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('auth: signup-flow', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('multiple users can sign up and each gets a working token', async ({ request }) => {
    const usernames = [randomUsername('userA'), randomUsername('userB')];

    const principals = await Promise.all(
      usernames.map((u) => signupUser(request, u, 'password1234')),
    );

    // それぞれ別 id / 別 token を持つこと。
    expect(principals[0].id).not.toBe(principals[1].id);
    expect(principals[0].token).not.toBe(principals[1].token);

    // 各 token が /api/i で正しい user を返すこと。
    for (const p of principals) {
      const resp = await callApi(request, 'i', { i: p.token });
      expect(resp.status()).toBe(200);
      const body = await resp.json();
      expect(body.id).toBe(p.id);
      expect(body.username).toBe(p.username);
    }
  });

  test('duplicate username is rejected', async ({ request }) => {
    const username = randomUsername('dupe');
    await signupUser(request, username, 'password1234');

    // upstream Misskey TS と mk-go (#798 status / #802 shape fix 後) は
    // status 400 + Fastify-style reply error
    // `{statusCode:400, error:"Bad Request", message:"Error: DUPLICATED_USERNAME"}`
    // を返す。本 spec は両者の strict shape を assert する。
    const resp = await callApi(request, 'signup', { username, password: 'password1234' });
    expect(resp.status()).toBe(400);
    const body = await resp.json();
    expect(body.statusCode).toBe(400);
    expect(body.error).toBe('Bad Request');
    expect(body.message).toBe('Error: DUPLICATED_USERNAME');
  });
});
