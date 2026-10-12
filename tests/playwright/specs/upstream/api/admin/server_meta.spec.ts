/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-E: admin/{meta, server-info, emoji/list-remote} read smoke。
//
// 3 endpoint:
//   - admin/meta: instance meta object (= drive caps / policies / etc)
//   - admin/server-info: hardware metadata
//   - admin/emoji/list-remote: 配列 (= remote emoji list)
//
// 全 admin / moderator 権限要、root token を再利用する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

test.describe('admin server / meta read shape', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('admin/meta returns object shape', async ({ request }) => {
    const resp = await callApi(request, 'admin/meta', { i: root.token });
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(typeof body).toBe('object');
    expect(body).not.toBeNull();
  });

  test('admin/server-info returns hardware metadata object', async ({ request }) => {
    const resp = await callApi(request, 'admin/server-info', { i: root.token });
    expect(resp.status()).toBe(200);
    const body = (await resp.json()) as Record<string, unknown>;
    // admin/server-info は public /api/server-info の machine/cpu/mem/fs に加え
    // os/node/psql/redis/net を返す (公開版は machine/cpu/mem/fs のみ)。
    expect(body.machine).toBeDefined();
    expect(body.cpu).toBeDefined();
    expect(body.mem).toBeDefined();
    expect(body.fs).toBeDefined();
    // admin 専用の拡張 field。
    expect(body.os).toBeDefined();
    expect(body.node).toBeDefined();
    expect(body.net).toBeDefined();
  });

  test('admin/emoji/list-remote returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'admin/emoji/list-remote', {
      i: root.token,
      limit: 5,
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });
});
