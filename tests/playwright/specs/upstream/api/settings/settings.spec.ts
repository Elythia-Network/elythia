/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #827: settings spec (security / API token / email)。
//
// upstream Misskey TS と mk-go は両方とも:
//   - i/change-password { currentPassword, newPassword } で password 更新
//     (204、failed = 403 INCORRECT_PASSWORD)
//   - i/regenerate-token { password } で API token 再発行 (= 2xx)
//   - i/update-email { password, email } で email 設定 (200 + Me)
//
// 本 spec は両 backend 共通で:
//   1. password change の round-trip (新 password で signin 成功)
//   2. token regenerate が 2xx で成功すること (= shape drift 抽象化)
//   3. email クリアの round-trip (200 + /api/i で null 反映)
//
// 2FA 系 (TOTP / passkey) は #817 part1/part2 で別 spec にて cover 済。
// notification 設定 (notificationRecieveConfig) は mk-go では read-only で
// 常に空 map を返しているため round-trip 化が困難 → 別 spec scope。
//
// drift fix history (closed):
//   - #883 i/regenerate-token return shape: TS=204 / mk-go=200+{token}
//     → mk-go を 204 に揃え (PR fix)
//   - #884 旧 API token invalidation: TS=即時 reject / mk-go=cache 経由で
//     旧 token が引き続き auth 通過していた security regression
//     → mk-go の auth cache から旧 token を即時削除 (PR fix)
//   - #885 password 検証失敗時の status: TS は endpoint ごとに 400/401 /
//     mk-go は一律 403 で揺れていた
//     → mk-go を endpoint 別に 401 (raw Error) / 400 (ApiError) に揃え
//
// 本 spec は drift fix 後の strict round-trip を担保する設計に更新済。
// 旧 token round-trip (regenerate 後の /api/i 4xx) も #884 fix の
// regression guard として追加。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('settings: security / API token / email', () => {
  // change-password 後に新 password で signin-flow を呼び戻すため、
  // 1h 5 回の signup 制限と signin 系制限を test 単位で reset する。
  test.beforeEach(() => {
    resetRateLimit();
  });

  test('change-password: new password succeeds via signin-flow', async ({
    request,
  }) => {
    const me = await signupUser(request, randomUsername('cpA'));
    const newPassword = 'np_' + Math.random().toString(16).slice(2, 10);

    // change-password
    const changeResp = await callApi(request, 'i/change-password', {
      i: me.token,
      currentPassword: DEFAULT_TEST_PASSWORD,
      newPassword,
    });
    expect(changeResp.status()).toBe(204);

    // signin-flow は per-IP の rate limit を共有しており、本 test の前段
    // で signupUser → change-password に既に複数 call を消費しているので、
    // 新 signin の前に counter を reset する (= 既存 spec の pattern)。
    resetRateLimit();

    // 新 password で signin → 200 + token (= drop-in shape として共通)
    const newSignin = await callApi(request, 'signin-flow', {
      username: me.username,
      password: newPassword,
    });
    expect(newSignin.status()).toBe(200);
    const body = await newSignin.json();
    expect(typeof body.i).toBe('string');
    expect(body.i.length).toBeGreaterThan(0);
  });

  test('regenerate-token: 204 No Content + old token immediately invalidated', async ({
    request,
  }) => {
    const me = await signupUser(request, randomUsername('rtA'));

    // upstream Misskey TS と mk-go (#883 / #884 fix 後) は両方とも 204 を
    // 返す drop-in shape。新 token は myTokenRegenerated WS event 経由で
    // client に通達される設計 (= body には含まない)。
    const regenResp = await callApi(request, 'i/regenerate-token', {
      i: me.token,
      password: DEFAULT_TEST_PASSWORD,
    });
    expect(regenResp.status()).toBe(204);

    // 旧 token は auth cache から即時削除され、以降 /api/i で 4xx
    // (= 401 等) で reject される (#884 security regression fix)。
    const oldI = await callApi(request, 'i', { i: me.token });
    expect(oldI.status()).toBeGreaterThanOrEqual(400);
    expect(oldI.status()).toBeLessThan(500);
  });

  test('update-email: clearing email returns 200 + null email in response', async ({
    request,
  }) => {
    const me = await signupUser(request, randomUsername('ueA'));

    // signup 直後は email 未設定 (= null)。先に何も操作せず /api/i で
    // 初期 null を確認。
    const before = await callApi(request, 'i', { i: me.token });
    expect(before.status()).toBe(200);
    const beforeBody = await before.json();
    expect(beforeBody.email ?? null).toBeNull();

    // 明示的に null を渡す path で 200 + 更新後 self entity (= Me)。
    // upstream Misskey TS と mk-go は両方とも emailRequiredForSignup=false
    // 設定なら null 許容、200 で更新 self を返す drop-in shape。
    const updResp = await callApi(request, 'i/update-email', {
      i: me.token,
      password: DEFAULT_TEST_PASSWORD,
      email: null,
    });
    expect(updResp.status()).toBe(200);
    const updBody = await updResp.json();
    expect(updBody.id).toBe(me.id);
    expect(updBody.email ?? null).toBeNull();

    // 別 call の /api/i でも email が null のまま反映されていること
    // (= update path と read path が同じ state を返す regression guard)。
    const after = await callApi(request, 'i', { i: me.token });
    expect(after.status()).toBe(200);
    const afterBody = await after.json();
    expect(afterBody.email ?? null).toBeNull();
  });
});
