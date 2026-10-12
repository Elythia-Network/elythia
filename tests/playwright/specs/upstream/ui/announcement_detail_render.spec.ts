/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /announcements/:id 単一 announcement 詳細 page で title + body が
// render されることを verify する mixed e2e。
//
// admin/announcements/create でテスト用 announcement を作って /announcements/:id
// を navigate、title が body に出ることを sign にする。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import type { RootFixture } from '../../../fixtures/ui_auth';

test.describe('UI: /announcements/:id renders single announcement', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('admin/announcements/create + /announcements/:id renders title', async ({
    page,
    baseURL,
    request,
  }) => {
    const title = `pwannd-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'admin/announcements/create', {
      i: root.token,
      title,
      text: 'playwright announcement detail body',
      imageUrl: null,
    });
    expect(createResp.status()).toBe(200);
    const announcement = await createResp.json();
    expect(announcement.id).toBeTruthy();

    // anonymous でも /announcements/:id は public 取得可能 (= announcements/show)
    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/announcements/${announcement.id}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      title,
      { timeout: 20_000 },
    );
  });
});
