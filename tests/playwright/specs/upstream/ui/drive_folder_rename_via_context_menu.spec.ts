/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/drive で folder を右クリック → context menu の "Rename" item
// (ti-fw ti-forms) → inputText dialog → 新名入力 → OK →
// /api/drive/folders/update が round-trip する write-flow spec。
//
// MkDrive.folder.vue:214 の rename() は os.inputText の resolve 後に
// misskeyApi('drive/folders/update', { folderId, name }) を呼ぶ
// (line 221)。drive_folder_delete_via_context_menu の sister として
// rename pattern を担保する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /my/drive folder rename via context menu flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('contextmenu folder → Rename → inputText OK → /api/drive/folders/update', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 folder を create via API
    const initialName = `pw-folder-rn-${Date.now()}`;
    const createResp = await callApi(request, 'drive/folders/create', {
      i: root.token,
      name: initialName,
    });
    expect(createResp.status()).toBe(200);
    const folderId = (await createResp.json()).id;
    expect(folderId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/my/drive`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      initialName,
      { timeout: 20_000 },
    );

    // 2. 該当 folder element に contextmenu event を dispatch。
    // MkDrive.folder.vue の root div は draggable="true" attribute を持つ
    // 唯一の (folder name text を含む) 要素。delete spec と同 pattern。
    await page.evaluate((n) => {
      const draggables = Array.from(
        document.querySelectorAll('[draggable="true"]'),
      ) as HTMLElement[];
      const target = draggables.find((el) => (el.textContent ?? '').includes(n));
      if (!target) return;
      target.dispatchEvent(
        new MouseEvent('contextmenu', {
          bubbles: true,
          cancelable: true,
          button: 2,
          buttons: 2,
          view: window,
        }),
      );
    }, initialName);

    // 3. popup menu の "Rename" item (ti-fw ti-forms) を click
    await clickButtonWithIcon(page, 'i.ti-fw.ti-forms');

    // 4. inputText dialog の text input が出るまで待つ
    await page.waitForFunction(
      () => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.type === 'text');
      },
      { timeout: 10_000 },
    );

    const newName = `pw-folder-renamed-${Date.now()}`;
    await page.evaluate((n) => {
      const inputs = (Array.from(document.querySelectorAll('input')) as HTMLInputElement[]).filter(
        (i) => i.type === 'text',
      );
      const target = inputs[inputs.length - 1];
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, n);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, newName);

    // 5. MkDialog OK → drive/folders/update
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/drive/folders/update') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await updateResp;

    // 6. API 経由で name 更新 verify
    const showResp = await callApi(request, 'drive/folders/show', {
      i: root.token,
      folderId,
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.id).toBe(folderId);
    expect(shown.name).toBe(newName);
  });
});
