/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/moderation の "Preserved usernames" MkFolder を expand →
// MkTextarea を編集 → Save click → /api/admin/update-meta が round-trip
// する write-flow spec。
//
// admin/moderation.vue:40-50 の同 folder は MkTextarea + Save MkButton primary
// の組。同 page には preservedUsernames / sensitiveWords / prohibitedWords
// 等 7 つの folder が並ぶが、それぞれ 1 textarea + 1 Save なので folder
// 単位で expand すれば識別可能。本 spec は preservedUsernames を代表として
// 取り、folder + textarea + save pattern を担保する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /admin/moderation preservedUsernames save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('expand folder → edit textarea → Save → /api/admin/update-meta', async ({
    page,
    baseURL,
    request,
  }) => {
    // setup: 既知 state (空 list) に reset。
    await callApi(request, 'admin/update-meta', {
      i: root.token,
      preservedUsernames: [],
    });

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/moderation`, {
        waitUntil: 'domcontentloaded',
      });

      // count 条件 (>= 5) ではなく target text を含む header の存在を直接
      // 待つ。folder hydrate 遅延で 60s test timeout する flake を回避。
      // 詳細は admin_moderation_blocked_hosts_save の同コメント参照。
      await page.waitForFunction(
        () => {
          const headers = Array.from(
            document.querySelectorAll('[data-testid="folder-header"]'),
          ) as HTMLElement[];
          return headers.some((h) =>
            (h.textContent ?? '').includes('Reserved usernames'),
          );
        },
        { timeout: 30_000 },
      );

      // "Preserved usernames" folder を expand
      await clickWhenReady(page, '「Reserved usernames」の folder-header', () => {
        const headers = Array.from(
          document.querySelectorAll('[data-testid="folder-header"]'),
        ) as HTMLElement[];
        const target = headers.find((h) =>
          (h.textContent ?? '').includes('Reserved usernames'),
        );
        return target;
      });

      // textarea が DOM に出るまで待つ
      await page.waitForFunction(
        () => document.querySelectorAll('textarea').length >= 1,
        { timeout: 10_000 },
      );

      // textarea の値を変更 (= modified=true → Save が effective)
      const newValue = `pwadmin\nadmin\nroot\nsystem\n${Date.now()}`;
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

      // Save button click → admin/update-meta
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
      // cleanup: preservedUsernames に "admin"/"root" 等が残ると以降の
      // signup spec で偶然 collision して失敗する可能性があるため空に戻す。
      await callApi(request, 'admin/update-meta', {
        i: root.token,
        preservedUsernames: [],
      });
    }
  });
});
