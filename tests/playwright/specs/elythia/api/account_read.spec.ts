/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 利用者の側の独自の API のうち、画面の spec で通っているが応答の型を見ていなかったもの
// (自分の連合の設定、通知なしで成立したフォロー) を叩き、応答を /api.json の宣言で照合する (#4093)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { callChecked, expectApiResponse } from '../../../fixtures/api_schema';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';

test.describe('account: the Elythia endpoints that read answer as declared', () => {
  test.skip(isTsBackend, 'ここで叩く口は Elythia 独自');

  test.beforeAll(() => {
    resetRateLimit();
  });

  test('my federation setting, read and turned off and on', async ({ request }) => {
    const user = await signupUser(request, randomUsername('accfd'));
    for (const [endpoint, body] of [
      ['i/federation', { i: user.token }],
      ['i/update-federation', { i: user.token, federationStatus: 'off' }],
      ['i/update-federation', { i: user.token, federationStatus: 'on' }],
    ] as const) {
      const resp = await callApi(request, endpoint, body);
      await expectApiResponse(request, endpoint, resp);
    }
  });

  test('a follow from a new account that succeeded silently', async ({ request }) => {
    const followee = await signupUser(request, randomUsername('accsf'));
    const follower = await signupUser(request, randomUsername('accsr'));
    // 作られて 1 日に満たないアカウントからのフォローは、通知なしで成立させる (#3466)
    const updated = await callApi(request, 'i/update', { i: followee.token, followApprovalLocalSeconds: 86_400, followApprovalAction: 'silentFollow' });
    expect(updated.status(), await updated.text()).toBe(200);
    const followed = await callApi(request, 'following/create', { i: follower.token, userId: followee.id });
    expect(followed.status(), await followed.text()).toBe(200);

    const silent = await callChecked<{ follower: { id: string } }[]>(request, 'following/silent/list', { i: followee.token });
    expect(silent.map((s) => s.follower.id)).toEqual([follower.id]);
  });
});
