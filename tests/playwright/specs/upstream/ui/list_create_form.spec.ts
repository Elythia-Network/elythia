/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/lists で "Create list" button click → MkDialog (input modal) で
// list 名入力 → OK click → users/lists/create が走り list が一覧に出る
// ことを verify する **真の write-flow** spec。
//
// MkDialog は data-cy-modal-dialog-ok / -cancel + input element を持つ。
// os.inputText は動的 popup なので、click 後に modal 出現を polling 待ち。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /my/lists "Create list" form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('click create → fill name → click OK → users/lists/create round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/my/lists`, { waitUntil: 'domcontentloaded' });

    // "Create list" button が hydrate するまで待つ
    // i18n.ts.createList → "Create list"。MkButton primary。
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button'));
        return btns.some((b) => (b.textContent ?? '').includes('Create list'));
      },
      { timeout: 20_000 },
    );

    // Create list button click → os.inputText popup (= MkDialog) が出る
    await clickButtonContainingText(page, 'Create list');

    // MkDialog input field 出現を polling 待ち。data-cy-modal-dialog-ok の
    // 兄弟要素として input が render される。
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    // input に list 名を書き込み (MkDialog の MkInput inner input)
    const listName = `pwlistui-${Date.now().toString().slice(-9)}`;
    await page.evaluate((n) => {
      // modal 内の最後の input が target (= MkDialog の MkInput)。
      // 複数 input がある場合 modal 内のものを取りたいので、
      // [data-testid="bg"] 内部の input を取る。
      const inputs = Array.from(
        document.querySelectorAll('input'),
      ) as HTMLInputElement[];
      // modal が表示された瞬間は modal 内 input が末尾に追加される
      const target = inputs[inputs.length - 1];
      target?.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, n);
      target?.dispatchEvent(new Event('input', { bubbles: true }));
    }, listName);

    // users/lists/create response 捕捉して OK click
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/users/lists/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await createResp;

    // list が一覧に追加されて render される (= /my/lists が cache reload)
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      listName,
      { timeout: 20_000 },
    );
  });
});
