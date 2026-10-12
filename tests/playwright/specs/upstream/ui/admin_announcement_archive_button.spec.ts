/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/announcements で 既存 announcement folder を expand →
// "End (Archive)" button click → /api/admin/announcements/update
// (isActive=false) round-trip する **真の write-flow** spec。
//
// admin/announcements.vue line 32: archive button text は
// `{{ end }} ({{ archive }})` (i18n.ts._announcement.end + i18n.ts.archive)。
// 該当 announcement folder を default open で render するために、API で
// 新規 (id 付き) announcement を作成 → /admin/announcements に navigate。
// 新作 announcement folder は MkFolder の defaultOpen logic で auto open
// しないので、folder header click が必要。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /admin/announcements archive button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('create via API → expand folder → click Archive → admin/announcements/update', async ({
    page,
    baseURL,
    request,
  }) => {
    const title = `pwann-arch-${Date.now().toString().slice(-9)}`;
    const text = `pwann-arch-body-${Date.now().toString().slice(-9)}`;
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

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/announcements`, { waitUntil: 'domcontentloaded' });

    // 新作 announcement の title が body に出るまで待つ (= list 反映)
    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      title,
      { timeout: 20_000 },
    );

    // 該当 folder の header をすべて click すると expand する。
    // textContent に title を含む button (= folder header) を click。
    await clickWhenReady(page, '対象 announcement の folder-header', (t) => {
      const headers = Array.from(
        document.querySelectorAll('[data-testid="folder-header"]'),
      ) as HTMLButtonElement[];
      const target = headers.find((h) => (h.textContent ?? '').includes(t));
      return target;
    }, title);

    // Archive button が visible になるまで待つ。textContent は
    // 'End (Archive)' を含む。
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button'));
        return btns.some((b) => (b.textContent ?? '').includes('Archive'));
      },
      { timeout: 10_000 },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/admin/announcements/update') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Archive');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
