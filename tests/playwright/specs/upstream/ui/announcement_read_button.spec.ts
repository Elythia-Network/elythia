/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /announcements で未読 announcement の "Got it!" button click →
// /api/i/read-announcement round-trip する **真の write-flow** spec。
//
// announcements.vue は tab !== 'past' && !announcement.isRead && !silence
// な announcement に "Got it!" button を出す (line 36-37)。click すると
// read() が i/read-announcement を呼ぶ (line 85)。
//
// 本 spec は admin/announcements/create で uniq title の unread
// announcement を作成 → /announcements に navigate → 該当 entry の Got
// it! button を click → API round-trip を verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /announcements read button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('admin creates → user reads via "Got it!" → /api/i/read-announcement', async ({
    page,
    baseURL,
    request,
  }) => {
    // admin として announcement を作成 (root は admin 権限)。
    const title = `pwann-${Date.now().toString().slice(-9)}`;
    const text = `pwann-body-${Date.now().toString().slice(-9)}`;
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
    await page.goto(`${baseURL}/announcements`, { waitUntil: 'domcontentloaded' });

    // 新 announcement の title が body に出るまで待つ。
    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      title,
      { timeout: 20_000 },
    );

    // i/read-announcement response 捕捉 → "Got it!" click。
    // /announcements の Got it! は MkButton primary で textContent に
    // "Got it!" を含む。同 page には別 popup (MkSourceCodeAvailablePopup)
    // も "Got it!" を持ちうるが、popup より先 (= 上) にある announcement
    // 内 button が DOM 順で先頭。
    const readResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/read-announcement') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Got it!');
    const resp = await readResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
