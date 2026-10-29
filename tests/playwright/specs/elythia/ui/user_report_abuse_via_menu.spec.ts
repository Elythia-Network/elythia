/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /@target の 3-dot menu → "Report abuse" item (ti-fw ti-exclamation-circle)
// → MkAbuseReportWindow popup → 定型フォーム入力 → Send button
// click → /api/users/report-abuse round-trip する write-flow spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { fillAbuseReportWindow, submitAbuseReportWindow } from '../../../fixtures/abuse_report';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: /@target abuse report flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('open menu → Report abuse → structured form + Send → /api/users/report-abuse', async ({
    page,
    baseURL,
    request,
  }) => {
    const target = await signupUser(request, `pwra${Date.now().toString().slice(-9)}`);
    expect(target.id).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/@${target.username}`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      (u) => document.body.textContent?.includes(u) ?? false,
      target.username,
      { timeout: 20_000 },
    );

    await clickButtonWithIcon(page, 'i.ti-dots');
    await clickButtonWithIcon(page, 'i.ti-fw.ti-exclamation-circle');

    const details = `pw-abuse-${Date.now()}`;
    await fillAbuseReportWindow(page, details);
    await submitAbuseReportWindow(page);
  });
});
