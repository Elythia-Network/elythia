/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-F: i/* 残 read 系 spec。
//
// 8 endpoint (drop-in 互換 target、両 backend 共通):
//   - i/notifications: 自分宛 notification list (= 配列)
//   - i/notifications-grouped: 同上 grouped 形式
//   - i/apps: 自分の access_token 一覧 (= 配列)
//   - i/authorized-apps: 自分が承認した 3rd party app 一覧 (= 配列)
//   - i/signin-history: signin 履歴 (= 配列)
//   - i/page-likes: 自分がいいねした page (= 配列)
//   - i/gallery/posts: 自分の gallery post (= 配列)
//   - i/gallery/likes: 自分がいいねした gallery (= 配列)
//
// scope 外 (= mk-go 拡張、upstream 未実装):
//   - i/flashs / i/flashs/likes: TS 上流に endpoint なし、mk-go 独自

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('i/* read shape compat', () => {
  // 1 user signup を beforeAll で再利用 (= signup rate limit 圧迫回避)。
  let userToken: string;

  test.beforeAll(async ({ request }) => {
    resetRateLimit();
    const me = await signupUser(request, randomUsername('iext'));
    userToken = me.token;
  });

  for (const endpoint of [
    'i/notifications',
    'i/notifications-grouped',
    'i/apps',
    'i/authorized-apps',
    'i/signin-history',
    'i/page-likes',
    'i/gallery/posts',
    'i/gallery/likes',
  ]) {
    test(`${endpoint} returns array shape`, async ({ request }) => {
      const resp = await callApi(request, endpoint, { i: userToken, limit: 5 });
      expect(resp.status()).toBe(200);
      expect(Array.isArray(await resp.json())).toBe(true);
    });
  }
});
