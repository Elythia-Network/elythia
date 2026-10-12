/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-D: admin/drive read 系。
//
// 2 endpoint:
//   - admin/drive/files: 配列 (= drive file list with pagination)
//   - admin/drive/show-file: 不明 fileId で 4xx LCD
//
// admin/drive/clean-remote-files / cleanup は destructive な mutation で
// remote file 全削除を伴うので spec scope 外、別 phase で扱う。
//
// admin 権限要、root token を再利用する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

test.describe('admin/drive read shape compat', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('admin/drive/files returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'admin/drive/files', {
      i: root.token,
      limit: 5,
      origin: 'combined',
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('admin/drive/show-file returns 4xx for unknown fileId', async ({ request }) => {
    // upstream paramDef format: 'misskey:id' の pre-validation か、
    // post-lookup での 404 のいずれか。LCD で吸収。
    const resp = await callApi(request, 'admin/drive/show-file', {
      i: root.token,
      fileId: '9zzzzzzzzzzzzzzz',
    });
    expect([400, 404]).toContain(resp.status());
  });
});
