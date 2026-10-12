/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /timeline default tab の初期 hydration smoke。upstream Misskey の /timeline
// route は store.r.tl.value.src で home / local / global / hybrid の最後に
// 選択した tab を render する。新規 root 認証直後は home tab がデフォルト
// で、root 自身の note は home timeline に乗る。本 spec は root が public
// note を post → /timeline navigate → note text が body に出るのを smoke。
//
// /timeline/local や /timeline/global は SPA route として独立しておらず、
// /timeline の中の tab 切替で表示される (= URL は変わらない)。よって本
// spec は default tab で見える note のみを verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /timeline renders a recent note from the viewer', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('a fresh public note from root appears in /timeline default tab', async ({
    page,
    baseURL,
    request,
  }) => {
    const noteText = `pwtl-${Date.now().toString().slice(-9)}`;
    const noteResp = await callApi(request, 'notes/create', {
      i: root.token,
      text: noteText,
      visibility: 'public',
    });
    expect(noteResp.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/timeline`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // note text が body に出る = MkStreamingNotesTimeline の hydration
    // (初期 fetch + MkNote render) が完了
    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      noteText,
      { timeout: 20_000 },
    );
  });
});
