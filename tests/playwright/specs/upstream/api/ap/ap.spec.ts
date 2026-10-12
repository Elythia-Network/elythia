/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #831: ap/show round-trip (local URI のみ)。
//
// Playwright stack は federation 設定なし (= 単一 local instance)。リモート
// AP object resolve は drop-in pytest が cover するので、本 spec は **local
// URI を ap/show が解決して User type を返す** shape のみを確認する。
//
// upstream Misskey TS と mk-go は両方とも:
//   - local user の `https://<host>/users/<id>` を渡すと
//     { type: 'User', object: { id, ... } } を返す

import { expect, test } from '@playwright/test';
import { callApi, baseURL } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('ap/show local URI resolve', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('ap/show resolves local user URI to type=User', async ({ request }) => {
    const me = await signupUser(request, randomUsername('ap'));

    // local user の AP URI = `<baseURL>/users/<id>`
    const userURI = `${baseURL()}/users/${me.id}`;
    const resp = await callApi(request, 'ap/show', {
      i: me.token,
      uri: userURI,
    });
    expect(resp.status()).toBe(200);
    const body = (await resp.json()) as { type?: string; object?: { id?: string } };
    expect(body.type).toBe('User');
    expect(body.object?.id).toBe(me.id);
  });
});
