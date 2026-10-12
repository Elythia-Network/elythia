/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #826: admin/show-user + suspend / unsuspend round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - admin/show-user (mod 必須) で対象 user の admin-only fields
//     (email / isModerator / isSuspended / signins / policies / roles 等)
//     を返す
//   - admin/suspend-user で isSuspended=true → 該当 user は signin 不可
//   - admin/unsuspend-user で isSuspended=false に戻し signin 可
//
// drop-in shape drift (= #888 で揃える方向):
//   - admin/show-user の top-level shape は backend 間で異なる (TS は admin
//     specific fields のみ / mk-go は UserDetailed + admin fields)。本 spec
//     は両 backend 共通の `isSuspended` boolean のみを strict assert し、
//     id / username の top-level field は scope 外。
//
// 本 spec は両 backend 共通で:
//   1. globalSetup root を admin として読み込み
//   2. 対象 user を signupUser で作成
//   3. admin/show-user で 200 + isSuspended=false を確認
//   4. admin/suspend-user → 204、対象が default password で signin 不可 (4xx)
//   5. admin/show-user で isSuspended=true 反映を確認
//   6. admin/unsuspend-user → 204、再度 signin 可 (200)
//
// signin-flow の rate limit が複数 signin で消費されるので、test 内で
// resetRateLimit を挟む (= 既存 settings spec と同 pattern)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

test.describe('admin: show-user / suspend / unsuspend round-trip', () => {
  test.beforeEach(() => {
    resetRateLimit();
  });

  test('admin shows user, suspends signin, then unsuspends', async ({
    request,
  }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    const target = await signupUser(request, randomUsername('auA'));

    // admin/show-user で対象 user を取得。両 backend 共通で確実に出る
    // field は admin-only の `isSuspended` (= TS 側は `id` / `username`
    // を含まない、mk-go は含むが drift)。本 spec は drop-in 共通な部分だけ
    // strict assert する。
    const showResp = await callApi(request, 'admin/show-user', {
      i: root.token,
      userId: target.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.isSuspended).toBe(false);

    // suspend
    const susp = await callApi(request, 'admin/suspend-user', {
      i: root.token,
      userId: target.id,
    });
    expect(susp.status()).toBe(204);

    // suspend 後 admin/show-user で isSuspended=true 反映確認
    const showAfterSusp = await callApi(request, 'admin/show-user', {
      i: root.token,
      userId: target.id,
    });
    expect(showAfterSusp.status()).toBe(200);
    const shownSusp = await showAfterSusp.json();
    expect(shownSusp.isSuspended).toBe(true);

    // signin-flow が ratelimit を消費している可能性があるので reset。
    resetRateLimit();

    // suspended user は signin できない (= 4xx range で reject)。
    const blocked = await callApi(request, 'signin-flow', {
      username: target.username,
      password: DEFAULT_TEST_PASSWORD,
    });
    expect(blocked.status()).toBeGreaterThanOrEqual(400);
    expect(blocked.status()).toBeLessThan(500);

    // unsuspend
    const unsusp = await callApi(request, 'admin/unsuspend-user', {
      i: root.token,
      userId: target.id,
    });
    expect(unsusp.status()).toBe(204);

    resetRateLimit();

    // 再度 signin → 200 + token
    const ok = await callApi(request, 'signin-flow', {
      username: target.username,
      password: DEFAULT_TEST_PASSWORD,
    });
    expect(ok.status()).toBe(200);
    const okBody = await ok.json();
    expect(typeof okBody.i).toBe('string');
  });
});
