/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/abuses で abuse report の "Resolve (accept)" button (ti-check) を
// click → /api/admin/resolve-abuse-user-report が round-trip する write-flow
// spec。
//
// MkAbuseReport.vue:20-22 で各 report に 3 つの resolve button (accept /
// reject / other) が並ぶ。accept は ti-check icon、reject は ti-x、other
// は ti-slash で識別可能。click すると os.apiWithDialog で
// admin/resolve-abuse-user-report を叩く (line 119)。
//
// setup: signupUser 2 名 (reporter + target) → reporter が target を
// report → root として /admin/abuses で resolve。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /admin/abuses report resolve flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('signup reporter+target → report → /admin/abuses → Resolve (accept) → /api/admin/resolve-abuse-user-report', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. reporter / target user を signup
    const reporter = await signupUser(request, `pwrep${Date.now().toString().slice(-9)}`);
    const target = await signupUser(request, `pwtgt${Date.now().toString().slice(-9)}`);
    expect(reporter.id).toBeTruthy();
    expect(target.id).toBeTruthy();

    // 2. reporter が target を abuse report
    const comment = `pw-abuse-resolve-${Date.now()}`;
    const reportResp = await callApi(request, 'users/report-abuse', {
      i: reporter.token,
      userId: target.id,
      comment,
    });
    expect(reportResp.status()).toBeLessThan(400);

    // 3. /admin/abuses を root として開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/abuses`, {
      waitUntil: 'domcontentloaded',
    });

    // report の comment が body に出るまで待つ (= list 反映)
    await page.waitForFunction(
      (c) => document.body.textContent?.includes(c) ?? false,
      comment,
      { timeout: 20_000 },
    );

    // 4. 該当 report の row を展開してから "Resolve (Accept)" を click。
    //
    // /admin/abuses の各 report は折り畳まれた MkFolder 行で、header は
    // `<button>` + `i.ti-exclamation-circle`。展開しない限り
    // "Resolve (Accept)" button は DOM に存在しない。
    //
    // 「最初の ti-check button を click」という旧実装は、ページ上唯一の
    // ti-check が AGPL source-code 告知 popup の "Got it!" だったため、
    // 無関係な button を押して API が飛ばず 15s timeout していた。
    await clickWhenReady(page, 'i.ti-exclamation-circle を持つボタン', (u) =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          b.querySelector('i.ti-exclamation-circle') !== null &&
          (b.textContent ?? '').includes(u),
      ),
      target.username,
    );

    await page.waitForFunction(
      () =>
        Array.from(document.querySelectorAll('button')).some(
          (b) => (b.textContent ?? '').trim() === 'Resolve (Accept)',
        ),
      { timeout: 15_000 },
    );

    const resolveResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/admin/resolve-abuse-user-report') &&
        r.status() < 300,
      { timeout: 15_000 },
    );
    await clickButtonByText(page, 'Resolve (Accept)');
    await resolveResp;
  });
});
