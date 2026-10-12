/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/announcements で archived announcement を expand → "Unarchive"
// button click → /api/admin/announcements/update (isActive=true) round-trip
// する write-flow spec。
//
// admin/announcements.vue:33 の unarchive button は v-if=isActive=false で
// 表示され、ti-restore icon + "Unarchive" text。click すると
// admin/announcements/update を isActive=true で叩く (line 184)。
// archive_button spec の inverse として用意。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickButtonContainingText, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /admin/announcements unarchive button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('create + archive via API → expand folder → click Unarchive → admin/announcements/update', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. announcement を create + archive (isActive=false) via API
    const title = `pwann-unarch-${Date.now().toString().slice(-9)}`;
    const text = `pwann-unarch-body-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'admin/announcements/create', {
      i: root.token,
      title,
      text,
      // upstream の paramDef は required: ['title','text','imageUrl'] なので
      // 省くと 400 INVALID_PARAM になる。mk-go は必須にしていないため
      // mk-go 単体では通っていたが、TS backend で落ちていた (#2276)。
      imageUrl: null,
    });
    expect(createResp.status()).toBe(200);
    const announcement = await createResp.json();
    const announcementId: string = announcement.id;
    expect(announcementId).toBeTruthy();

    // archive (= isActive=false に切替) via API
    const archiveResp = await callApi(request, 'admin/announcements/update', {
      i: root.token,
      id: announcementId,
      title,
      text,
      isActive: false,
      icon: announcement.icon ?? 'info',
      display: announcement.display ?? 'normal',
      forExistingUsers: announcement.forExistingUsers ?? false,
      silence: announcement.silence ?? false,
      needConfirmationToRead: announcement.needConfirmationToRead ?? false,
      imageUrl: announcement.imageUrl ?? null,
    });
    expect(archiveResp.status()).toBeLessThan(400);

    // 2. /admin/announcements を開く。
    // admin/announcements.vue:118 の MkSelect は **`initialValue: 'active'`**
    // で active list を表示するため、archived 状態の announcement は
    // default で list に出ない。MkSelect (= "Active" container) を click
    // して "Archived" filter に切替えてから target を待つ必要がある。
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/announcements`, {
      waitUntil: 'domcontentloaded',
    });

    // 3. MkSelect container hydrate 待ち。MkSelect.vue:11 は tabindex="0" を
    // 持つ div で current value text (= "Active") を含む構造で render する。
    // attribute selector で MkSelect 候補に絞り込んでから text 一致を見る
    // (page 上の別 div で textContent === 'Active' な要素との誤 hit を回避)。
    await page.waitForFunction(
      () => {
        const els = Array.from(document.querySelectorAll('div[tabindex="0"]'));
        return els.some((el) => (el.textContent ?? '').trim() === 'Active');
      },
      { timeout: 20_000 },
    );

    // 4. MkSelect は MkSelect.vue:15 で `@mousedown.prevent="show"` listener
    // のため click event では popup が出ない。mousedown を dispatch する。
    // (= MkNote footer button と同 pattern)
    await page.evaluate(() => {
      const els = Array.from(
        document.querySelectorAll('div[tabindex="0"]'),
      ) as HTMLDivElement[];
      const target = els.find((el) => (el.textContent ?? '').trim() === 'Active');
      target?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }));
    });

    // 5. popupMenu の "Archived" item を click。MkMenu の menu item は
    // 標準 click で OK (MkMenu.vue:151 の @click.prevent)。
    await clickButtonByText(page, 'Archived');

    // 6. archived list が hydrate して target title が body に出るのを待つ
    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      title,
      { timeout: 20_000 },
    );

    // 3. 該当 folder を expand
    await clickWhenReady(page, '対象 announcement の folder-header', (t) => {
      const headers = Array.from(
        document.querySelectorAll('[data-testid="folder-header"]'),
      ) as HTMLButtonElement[];
      const target = headers.find((h) => (h.textContent ?? '').includes(t));
      return target;
    }, title);

    // Unarchive button が visible になるまで待つ
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button'));
        return btns.some((b) => (b.textContent ?? '').includes('Unarchive'));
      },
      { timeout: 10_000 },
    );

    // 4. Unarchive click → admin/announcements/update (isActive=true) round-trip
    const updateResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/admin/announcements/update') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Unarchive');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);

    // 5. API 経由で isActive=true verify
    const listResp = await callApi(request, 'admin/announcements/list', {
      i: root.token,
      limit: 50,
    });
    expect(listResp.status()).toBe(200);
    const list = await listResp.json();
    const found = list.find((a: { id: string }) => a.id === announcementId);
    expect(found).toBeTruthy();
    expect(found.isActive).toBe(true);
  });
});
