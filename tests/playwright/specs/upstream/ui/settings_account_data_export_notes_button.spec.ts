/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/account-data の "Export notes" button click → /api/i/export-notes
// が round-trip する write-flow spec。
//
// account-data.vue:21 の MkButton primary は ti-download icon + "Export"
// text。click すると misskeyApi('i/export-notes', {}) を直接叩く (line 200)。
// page 内には export* / import* button が複数あるが、各 section は MkFolder
// で囲まれ、最初の section が "Notes" なので 1 番目の Export button が
// exportNotes に対応する。
//
// download / dialog 演出は無く、success alert が即出る。本 spec は API
// round-trip を verify する形。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/account-data export notes button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('first Export button → /api/i/export-notes', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/account-data`, {
      waitUntil: 'domcontentloaded',
    });

    // account-data.vue は SearchMarker > outer MkFolder (closed) > inner
    // MkFolder defaultOpen > Export button という入れ子構造。outer MkFolder は
    // `openedAtLeastOnce` false 初期値で content を lazy mount するため、
    // 一度開かない限り Export button は DOM に存在しない (#979 fix)。
    // "All notes" outer folder を expand する。
    await page.waitForFunction(
      () => {
        const headers = Array.from(
          document.querySelectorAll('[data-testid="folder-header"]'),
        ) as HTMLElement[];
        return headers.some((h) =>
          (h.textContent ?? '').includes('All notes'),
        );
      },
      { timeout: 20_000 },
    );
    await clickWhenReady(page, '「All notes」の folder-header', () => {
      const headers = Array.from(
        document.querySelectorAll('[data-testid="folder-header"]'),
      ) as HTMLElement[];
      const target = headers.find((h) =>
        (h.textContent ?? '').includes('All notes'),
      );
      return target;
    });

    // expand 後、Export button (= ti-download icon を持つ button) が mount
    // するまで待つ
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some((b) => b.querySelector('i.ti-download') !== null);
      },
      { timeout: 15_000 },
    );

    // 最初の "Export" button (= "Notes" section の export) を click。
    //
    // 内側の MkFolder は label が "Export" / icon が ti-download なので、
    // **folder header button 自身**も `ti-download` + "Export" に match して
    // しまう。しかも DOM 上こちらが先に来るため、素朴な find では folder の
    // 開閉を toggle するだけで i/export-notes は永久に飛ばなかった。
    // `data-testid="folder-header"` を除外して本物の MkButton を取る。
    const exportResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/export-notes') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '「Export」のボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          b.querySelector('i.ti-download') !== null &&
          (b.textContent ?? '').includes('Export') &&
          b.closest('[data-testid="folder-header"]') === null,
      ),
    );
    await exportResp;
  });
});
