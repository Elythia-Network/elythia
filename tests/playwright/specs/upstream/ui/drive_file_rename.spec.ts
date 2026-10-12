/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/drive/file/:fileId で rename ボタンを click → MkDialog の
// inputText で新しい file 名を入れて OK → /api/drive/files/update が
// 走り、API GET で名前が更新されていることを verify する write-flow spec。
//
// drive.file.info.vue の rename() は os.inputText の resolve 後に
// os.apiWithDialog('drive/files/update', { fileId, name }) を呼ぶ。
// dialog は単一の MkInput type='text' 1 個だけなので、現れた input に
// 値を投入して MkDialog の OK を click すれば round-trip が走る。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { uploadTinyPNG } from '../../../fixtures/files';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /my/drive/file/:fileId rename round-trip', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('rename button → inputText dialog → drive/files/update', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 drive file を upload
    const file = await uploadTinyPNG(request, baseURL!, root.token, `pw-rename-${Date.now()}.png`);
    const fileId = file.id;
    const oldName = file.name;

    // 2. detail page を開いて hydrate を待つ
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/drive/file/${fileId}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      oldName,
      { timeout: 20_000 },
    );

    // 3. rename ボタンを click。drive.file.info.vue の rename() ボタンは
    // 「<h2> file name + <i.ti-pencil>」を子に持つ唯一の <button>。
    // vite hash で class セレクタは production で死ぬので、構造で当てる。
    await clickWhenReady(page, 'i.ti-pencil を持つボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) => b.querySelector('h2') !== null && b.querySelector('i.ti-pencil') !== null,
      ),
    );

    // 4. inputText dialog (= MkDialog with single MkInput type='text') を待つ
    await page.waitForFunction(
      () => {
        const inputs = Array.from(
          document.querySelectorAll('input'),
        ) as HTMLInputElement[];
        return inputs.some((i) => i.type === 'text');
      },
      { timeout: 10_000 },
    );

    // 5. 新しい file 名を input に投入
    const newName = `pw-renamed-${Date.now()}.png`;
    await page.evaluate((name) => {
      const inputs = Array.from(
        document.querySelectorAll('input'),
      ) as HTMLInputElement[];
      // dialog 内 input は最後に mount された text input。MkSuperMenu の
      // search input (= settings layout) と被らないよう type=text のみ抽出。
      const target = inputs.filter((i) => i.type === 'text').pop();
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, name);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, newName);

    // 6. MkDialog の OK button (data-cy-modal-dialog-ok) を click → update 呼出
    const updateResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/drive/files/update') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    const update = await updateResp;
    expect(update.status()).toBeLessThan(300);

    // 7. API 経由で round-trip を verify
    const showResp = await request.post(`${baseURL}/api/drive/files/show`, {
      ignoreHTTPSErrors: true,
      data: { i: root.token, fileId },
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.id).toBe(fileId);
    expect(shown.name).toBe(newName);
  });
});
