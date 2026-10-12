/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #826: admin/update-meta + /api/meta round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - /api/meta (auth 不要) で instance meta (name, description, ...) を返す
//   - admin/update-meta { ... } で admin 権限で meta を更新 (204)
//   - 更新後 /api/meta に新値が反映される
//
// 本 spec は両 backend 共通で:
//   1. /api/meta で初期 name を取得
//   2. admin/update-meta で name を unique 値に変更
//   3. /api/meta 再取得で新 name 反映を確認
//   4. afterEach で元の name に restore (= 後続 spec の noise を防ぐ)
//
// admin/update-meta は instance global state を変更するため、cleanup を
// 慎重に行う (= settings spec / emoji_lifecycle spec と同 pattern)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

test.describe('admin: update-meta + /api/meta round-trip', () => {
  let rootToken: string | undefined;
  let originalName: string | null | undefined;

  test.beforeAll(() => {
    resetRateLimit();
  });

  test.afterEach(async ({ request }) => {
    if (rootToken && originalName !== undefined) {
      // restore (instance global state を test 前に戻す)。null は upstream
      // の "未設定" を表す可能性があるため、空文字 / null どちらでも復元
      // できるよう conditional payload で送る。
      const body: Record<string, unknown> = { i: rootToken };
      body.name = originalName;
      await callApi(request, 'admin/update-meta', body);
    }
    rootToken = undefined;
    originalName = undefined;
  });

  test('admin updates meta name and /api/meta reflects', async ({
    request,
  }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    rootToken = root.token;

    // 初期 /api/meta を取得 (auth 不要)
    const before = await callApi(request, 'meta', {});
    expect(before.status()).toBe(200);
    const beforeBody = await before.json();
    originalName = beforeBody.name ?? null;

    // unique な name を設定
    const newName = 'spec-meta-' + Math.random().toString(16).slice(2, 8);
    const updResp = await callApi(request, 'admin/update-meta', {
      i: root.token,
      name: newName,
    });
    expect(updResp.status()).toBe(204);

    // /api/meta 再取得で反映確認
    const after = await callApi(request, 'meta', {});
    expect(after.status()).toBe(200);
    const afterBody = await after.json();
    expect(afterBody.name).toBe(newName);
  });
});
