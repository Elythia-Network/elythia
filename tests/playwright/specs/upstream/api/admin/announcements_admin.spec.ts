/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-E: admin/announcements の list / update を補強。
//
// 既存 spec (#909 announcements/announcements.spec.ts) で create / delete /
// /api/announcements / i/read-announcement が cover 済。本 spec は admin
// 側の list と update を埋める:
//   - admin/announcements/list: 配列 (= 全 announcement、archived 含む)
//   - admin/announcements/update: title / text 等を変更

import { randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

interface Announcement {
  id: string;
  title: string;
  text: string;
}

test.describe('admin/announcements/* list + update', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('create → list → update → delete round-trip', async ({ request }) => {
    const title = `spec_admin_ann_${randomUUID()}`;

    // 1. create (= 既存 spec と同じ)
    const createResp = await callApi(request, 'admin/announcements/create', {
      i: root.token,
      title,
      text: 'phase4 spec',
      imageUrl: null,
      icon: 'info',
      display: 'normal',
    });
    expect(createResp.status()).toBe(200);
    const created = (await createResp.json()) as Announcement;
    const announcementId = created.id;

    // 2. admin/announcements/list で含まれる
    const listResp = await callApi(request, 'admin/announcements/list', {
      i: root.token,
      limit: 50,
    });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as Announcement[];
    expect(Array.isArray(list)).toBe(true);
    expect(list.find((a) => a.id === announcementId)).toBeDefined();

    // 3. update で text を変更
    const updResp = await callApi(request, 'admin/announcements/update', {
      i: root.token,
      id: announcementId,
      title,
      text: 'updated by spec',
      imageUrl: null,
      icon: 'info',
      display: 'normal',
    });
    expect([200, 204]).toContain(updResp.status());

    // 4. cleanup
    const delResp = await callApi(request, 'admin/announcements/delete', {
      i: root.token,
      id: announcementId,
    });
    expect([200, 204]).toContain(delResp.status());
  });
});
