/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #835: announcements list / read round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - admin/announcements/create で announcement を作成 (admin only)
//   - /api/announcements (auth) で自分宛 + global の active announcement を list
//   - i/read-announcement で既読化 (= announcement_read row 作成)
//   - list 再取得で isRead が反映されること
//
// admin CRUD 自体は #826 で別 cover、本 spec は public list + read 経路に集中する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
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
  isRead?: boolean;
}

test.describe('announcements list + read round-trip', () => {
  let createdAnnouncementId: string | undefined;
  let rootToken: string | undefined;

  test.beforeAll(() => {
    resetRateLimit();
  });

  test.afterEach(async ({ request }) => {
    if (createdAnnouncementId && rootToken) {
      await callApi(request, 'admin/announcements/delete', {
        i: rootToken,
        id: createdAnnouncementId,
      });
    }
    createdAnnouncementId = undefined;
    rootToken = undefined;
  });

  test('admin creates global announcement → user lists / reads it', async ({ request }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    rootToken = root.token;
    const me = await signupUser(request, randomUsername('annA'));

    const title = `spec_ann_${Math.random().toString(16).slice(2, 8)}`;
    // admin が global announcement を作成
    const createResp = await callApi(request, 'admin/announcements/create', {
      i: root.token,
      title,
      text: 'spec body',
      imageUrl: null,
      icon: 'info',
      display: 'normal',
    });
    expect(createResp.status()).toBe(200);
    const created = (await createResp.json()) as Announcement;
    createdAnnouncementId = created.id;

    // user が announcements を list
    const listResp = await callApi(request, 'announcements', { i: me.token });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as Announcement[];
    const found = list.find((a) => a.id === created.id);
    expect(found).toBeDefined();
    expect(found?.title).toBe(title);
    // 初期は未読
    expect(found?.isRead).toBeFalsy();

    // i/read-announcement で既読化
    const readResp = await callApi(request, 'i/read-announcement', {
      i: me.token,
      announcementId: created.id,
    });
    expect([200, 204]).toContain(readResp.status());

    // list 再取得で isRead=true
    const listAfter = await callApi(request, 'announcements', { i: me.token });
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as Announcement[];
    const foundAfter = listAfterBody.find((a) => a.id === created.id);
    expect(foundAfter?.isRead).toBe(true);
  });
});
