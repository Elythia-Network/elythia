/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/moderation の "Sensitive words" MkFolder を expand → MkTextarea
// を編集 → Save → /api/admin/update-meta が round-trip する write-flow
// spec。folder + textarea + save pattern の更なる sister。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /admin/moderation sensitiveWords save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('expand Sensitive words folder → edit textarea → Save → /api/admin/update-meta', async ({
    page,
    baseURL,
    request,
  }) => {
    // setup: 既知 state (空 list) に reset。
    await callApi(request, 'admin/update-meta', {
      i: root.token,
      sensitiveWords: [],
    });

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/moderation`, {
        waitUntil: 'domcontentloaded',
      });

      await page.waitForFunction(
        () => document.querySelectorAll('[data-testid="folder-header"]').length >= 5,
        { timeout: 20_000 },
      );

      // "Sensitive words" folder を expand
      await clickWhenReady(page, '「Sensitive words」の folder-header', () => {
        const headers = Array.from(
          document.querySelectorAll('[data-testid="folder-header"]'),
        ) as HTMLElement[];
        const target = headers.find((h) =>
          (h.textContent ?? '').includes('Sensitive words'),
        );
        return target;
      });

      await page.waitForFunction(
        () => document.querySelectorAll('textarea').length >= 1,
        { timeout: 10_000 },
      );

      const newValue = `pwsens-${Date.now().toString().slice(-9)}\nadult\nnsfw`;
      await page.evaluate((v) => {
        const tas = Array.from(document.querySelectorAll('textarea')) as HTMLTextAreaElement[];
        const target = tas[0];
        if (!target) return;
        target.focus();
        const setter = Object.getOwnPropertyDescriptor(
          window.HTMLTextAreaElement.prototype,
          'value',
        )?.set;
        setter?.call(target, v);
        target.dispatchEvent(new Event('input', { bubbles: true }));
      }, newValue);

      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/admin/update-meta') && r.status() < 300,
        { timeout: 15_000 },
      );
      await clickWhenReady(page, '「Save」のボタン', () =>
        Array.from(document.querySelectorAll('button')).find(
          (b) => !b.disabled && (b.textContent ?? '').includes('Save'),
        ),
      );
      await updateResp;
    } finally {
      // cleanup: sensitiveWords が残っても他 spec への直接影響は無いが、
      // test isolation のため空に戻す。
      await callApi(request, 'admin/update-meta', {
        i: root.token,
        sensitiveWords: [],
      });
    }
  });
});
