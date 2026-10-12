/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #820: users/show + i/update の round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - i/update で `name` / `description` 等の自プロフィール field を更新
//   - users/show { userId } で更新後の値が反映される (= 同 transaction で
//     即可視)
//
// 本 spec は両 backend 共通で:
//   1. user signup
//   2. users/show 初期状態 → name=null / description=null を確認
//   3. i/update で name + description を設定
//   4. users/show 再取得 → 更新値が反映されること
//
// avatar / banner 等の file 系 field 更新は別 spec (drive 系) で扱う。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('users: profile show / i/update round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('i/update reflects via users/show', async ({ request }) => {
    const me = await signupUser(request, randomUsername('upA'));

    // 初期状態は name / description ともに未設定 (null)。
    const before = await callApi(request, 'users/show', { userId: me.id });
    expect(before.status()).toBe(200);
    const beforeBody = await before.json();
    expect(beforeBody.id).toBe(me.id);
    // 未設定 field は null (= upstream TS と mk-go の packUserDetailed shape)
    expect(beforeBody.name).toBeNull();
    expect(beforeBody.description).toBeNull();

    // 更新を投げる。i/update は 200 で更新後の self entity を返す。
    const updateResp = await callApi(request, 'i/update', {
      i: me.token,
      name: 'Alice',
      description: 'hello playwright',
    });
    expect(updateResp.status()).toBe(200);
    const updated = await updateResp.json();
    expect(updated.name).toBe('Alice');
    expect(updated.description).toBe('hello playwright');

    // users/show 再取得で更新が反映されていること。
    const after = await callApi(request, 'users/show', { userId: me.id });
    expect(after.status()).toBe(200);
    const afterBody = await after.json();
    expect(afterBody.id).toBe(me.id);
    expect(afterBody.name).toBe('Alice');
    expect(afterBody.description).toBe('hello playwright');
  });
});
