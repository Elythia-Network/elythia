/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/announcements で header "Add" button → 新規 MkFolder 出現 →
// title MkInput + text MkTextarea に書き込み → Save click →
// /api/admin/announcements/create round-trip → 一覧に反映される
// **真の write-flow** spec。
//
// header actions は icon-only button (ti ti-plus + tooltip "Add")。
// Save は MkButton primary で textContent "Save" を含む。
// admin/announcements/create は title / text 両方 required (handler.go:196)。
// os.apiWithDialog は成功 dialog を出すので、API response 捕捉で完了判定。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText, clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: /admin/announcements create form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('click + → fill title → Save → admin/announcements/create round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/announcements`, { waitUntil: 'domcontentloaded' });

    // header の "+" button (ti-plus) が hydrate するまで待つ
    await page.waitForFunction(
      () => document.querySelector('button i.ti-plus') !== null,
      { timeout: 20_000 },
    );

    // "+" header action click → 新規 announcement folder が unshift される
    await clickButtonWithIcon(page, 'i.ti-plus');

    // 新 folder の title MkInput が hydrate するまで待つ
    // (default title "New announcement" が初期値で入っている)
    await page.waitForFunction(
      () => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === 'New announcement');
      },
      { timeout: 10_000 },
    );

    // 新 folder の title input は value="New announcement" の input。
    // unique title に書き換える。
    const announcementTitle = `pwannui-${Date.now().toString().slice(-9)}`;
    const announcementText = `pwannui-body-${Date.now().toString().slice(-9)}`;
    await page.evaluate(
      ({ t, b }) => {
        // title input (= value="New announcement")
        const titleInput = (
          Array.from(document.querySelectorAll('input')) as HTMLInputElement[]
        ).find((i) => i.value === 'New announcement');
        if (!titleInput) return;
        titleInput.focus();
        const inputSetter = Object.getOwnPropertyDescriptor(
          window.HTMLInputElement.prototype,
          'value',
        )?.set;
        inputSetter?.call(titleInput, t);
        titleInput.dispatchEvent(new Event('input', { bubbles: true }));

        // text textarea は page 全体の first textarea でよい。本 page
        // (/admin/announcements) は announcement folder 群の MkTextarea
        // しか textarea を持たず、新 folder は unshift で最上位に追加される
        // ため document.querySelector('textarea') は確定で新 folder のもの。
        // CSS module が production build で `xxKsY` 等の hash 名に潰れる
        // ため `[class*="MkFolder"]` での scope 限定は機能しない (vite
        // generateScopedName, #744 batch3 review)。
        const textarea = document.querySelector('textarea') as HTMLTextAreaElement | null;
        if (textarea) {
          textarea.focus();
          const taSetter = Object.getOwnPropertyDescriptor(
            window.HTMLTextAreaElement.prototype,
            'value',
          )?.set;
          taSetter?.call(textarea, b);
          textarea.dispatchEvent(new Event('input', { bubbles: true }));
        }
      },
      { t: announcementTitle, b: announcementText },
    );

    // admin/announcements/create response 捕捉して Save click
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/admin/announcements/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    // 最初の Save button (= 新規 folder 内) を click。
    // 既存 announcement folder は default closed なので Save button は
    // 新 folder の 1 個だけ visible/clickable。
    await clickButtonContainingText(page, 'Save');
    await createResp;

    // refresh() で list 再取得 → 新 announcement の title が一覧に出る
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      announcementTitle,
      { timeout: 20_000 },
    );
  });
});
