/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /play/:id (flash 詳細 page) で flash/create で作成した flash の title /
// summary が hydrate されることを verify する spec。
//
// upstream Misskey の flash route は `/play/:id` (= AiScript playground)。
// flash/show 経由で title / summary / script を取得して MkFlashPage の中
// で render する。本 spec は title / summary を body match で smoke する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import type { RootFixture } from '../../../fixtures/ui_auth';

test.describe('UI: /play/:id flash detail page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('flash/create + /play/:id renders title and summary', async ({ page, baseURL, request }) => {
    const title = `pwflash-${Date.now().toString().slice(-9)}`;
    const summary = `pwflash-summary-${Date.now().toString().slice(-9)}`;

    const createResp = await callApi(request, 'flash/create', {
      i: root.token,
      title,
      summary,
      script: '<:: "hello world"',
      permissions: [],
      visibility: 'public',
    });
    expect(createResp.status()).toBe(200);
    const flash = await createResp.json();
    expect(flash.id, 'flash/create should return id').toBeTruthy();

    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/play/${flash.id}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // title + summary が body に出る (= MkFlashPage の hydration 完了)
    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      title,
      { timeout: 20_000 },
    );
    await page.waitForFunction(
      (s) => document.body.textContent?.includes(s) ?? false,
      summary,
      { timeout: 20_000 },
    );
  });
});
