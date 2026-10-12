/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/job-queue page の hydration smoke。
//
// PageWithHeader の tabs は misskey-js の `queueTypes` 定数 (= deliver /
// inbox / db / system / objectStorage / ...) を hardcode で展開するので、
// queue inspector が wire されていない test 環境でも tab 名は必ず render
// される。"deliver" + "inbox" の両 tab 名が body に出るのを hydration sign
// にする (= MkPaginatedHeader が tabs prop を mount できた証拠)。
//
// 注: queue overview の Active/Delayed/Waiting 数値は admin/queue/queues の
// API から populate されるが、Playwright stack では queueInspector が
// 未配線で空配列を返すため body には現れない。本 smoke は frontend chain
// だけを cover し、API 値 verify は別 spec (specs/admin/queue.spec.ts)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/job-queue page hydrates queue tab list', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('queue type tabs (deliver / inbox) appear on /admin/job-queue', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/job-queue`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // header tabs は misskey-js queueTypes から生成され、deliver / inbox
    // は mk-go / Misskey TS の両 backend で必ず存在する標準 queue。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('deliver') && text.includes('inbox');
      },
      { timeout: 20_000 },
    );
  });
});
