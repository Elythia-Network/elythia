/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type { Page } from '@playwright/test';
import { clickWhenReady } from './ui_click';

/** MkAbuseReportWindow の定型フォームを埋めて Send 可能にする。 */
export async function fillAbuseReportWindow(page: Page, details: string): Promise<void> {
  await page.waitForFunction(
    () => document.querySelector('[data-testid="abuse-report-details"] textarea') !== null,
    { timeout: 10_000 },
  );

  // MkSelect: mousedown で category ドロップダウンを開く
  await page.evaluate(() => {
    const chevrons = Array.from(document.querySelectorAll('i.ti-chevron-down'));
    const chevron = chevrons[chevrons.length - 1];
    const select = chevron?.closest('[tabindex="0"]') as HTMLElement | null;
    select?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }));
  });

  await page.waitForFunction(
    () => {
      const buttons = Array.from(document.querySelectorAll('button'));
      return buttons.some((b) => /spam|スパム/i.test(b.textContent ?? ''));
    },
    { timeout: 10_000 },
  );

  await page.evaluate(() => {
    const buttons = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
    const option = buttons.find((b) => /spam|スパム/i.test(b.textContent ?? ''));
    option?.click();
  });

  await page.evaluate((text) => {
    const textarea = document.querySelector(
      '[data-testid="abuse-report-details"] textarea',
    ) as HTMLTextAreaElement | null;
    if (!textarea) return;
    textarea.focus();
    const setter = Object.getOwnPropertyDescriptor(
      window.HTMLTextAreaElement.prototype,
      'value',
    )?.set;
    setter?.call(textarea, text);
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
  }, details);

  await page.waitForFunction(
    () => {
      const buttons = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
      return buttons.some(
        (b) => !b.disabled && (b.textContent ?? '').trim().match(/^Send$|^送信$/i),
      );
    },
    { timeout: 10_000 },
  );
}

export async function submitAbuseReportWindow(page: Page): Promise<void> {
  const reportResp = page.waitForResponse(
    (r) => r.url().includes('/api/users/report-abuse') && r.status() < 300,
    { timeout: 15_000 },
  );
  await clickWhenReady(page, 'Send ボタン', () =>
    Array.from(document.querySelectorAll('button')).find(
      (b) => !b.disabled && (b.textContent ?? '').trim().match(/^Send$|^送信$/i),
    ),
  );
  await reportResp;
}
