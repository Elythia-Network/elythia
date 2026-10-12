/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 他人の note を /notes/:id で開いて 3-dot menu → "Report abuse"
// → MkAbuseReportWindow → 定型フォーム送信 → /api/users/report-abuse。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { fillAbuseReportWindow, submitAbuseReportWindow } from '../../../fixtures/abuse_report';
import { signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: note 3-dot menu abuse report flow', () => {
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
    const target = await signupUser(request, `pwnt${Date.now().toString().slice(-9)}`);
    const noteText = `pw-note-report-${Date.now()}`;
    const note = await createNote(request, target.token, { text: noteText, visibility: 'public' });

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/notes/${note.id}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      noteText,
      { timeout: 20_000 },
    );

    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some((b) => b.querySelector('i.ti-dots') !== null);
      },
      { timeout: 15_000 },
    );
    await page.evaluate(() => {
      const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
      const targetBtn = btns.find((b) => b.querySelector('i.ti-dots') !== null);
      targetBtn?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }));
    });

    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some((b) => b.querySelector('i.ti-fw.ti-exclamation-circle') !== null);
      },
      { timeout: 10_000 },
    );
    await clickButtonWithIcon(page, 'i.ti-fw.ti-exclamation-circle');

    const details = `pw-note-abuse-${Date.now()}`;
    await fillAbuseReportWindow(page, details);
    await submitAbuseReportWindow(page);
  });
});
