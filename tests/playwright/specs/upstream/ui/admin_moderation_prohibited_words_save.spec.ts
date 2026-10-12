/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/moderation の "Prohibited words" MkFolder を expand → MkTextarea
// を編集 → primary Save click → /api/admin/update-meta が round-trip
// する write-flow spec。
//
// admin_moderation_preserved_usernames_save の sister。同 page の
// 7 folder のうち別 section (prohibitedWords) で folder + textarea +
// save pattern が機能することを担保する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /admin/moderation prohibitedWords save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('expand Prohibited words folder → edit textarea → Save → /api/admin/update-meta', async ({
    page,
    baseURL,
    request,
  }) => {
    // setup: 既知 state (空 list) に reset。
    await callApi(request, 'admin/update-meta', {
      i: root.token,
      prohibitedWords: [],
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

      // "Prohibited words" folder を expand。"Prohibited words" は
      // "Prohibited words for username" と prefix が被るので、textContent が
      // "Prohibited words" で始まり "username" を含まないものを選ぶ。
      await clickWhenReady(page, '「Prohibited words」の folder-header', () => {
        const headers = Array.from(
          document.querySelectorAll('[data-testid="folder-header"]'),
        ) as HTMLElement[];
        const target = headers.find((h) => {
          const t = (h.textContent ?? '').trim();
          return t.startsWith('Prohibited words') && !t.toLowerCase().includes('username');
        });
        return target;
      });

      await page.waitForFunction(
        () => document.querySelectorAll('textarea').length >= 1,
        { timeout: 10_000 },
      );

      const newValue = `bad-word-${Date.now()}\noffensive\nspam`;
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
      // cleanup: prohibitedWords が残ると以降の note 投稿 spec が "Note
      // contains prohibited words" で fail する isolation 破壊を引き起こす
      // ため、必ず空に戻す。pass / fail どちらでも cleanup を実行。
      await callApi(request, 'admin/update-meta', {
        i: root.token,
        prohibitedWords: [],
      });
    }
  });
});
