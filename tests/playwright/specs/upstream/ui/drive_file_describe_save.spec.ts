/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/drive/file/:fileId で description button (kvEditBtn) → MkModalWindow
// (MkFileCaptionEditWindow) → caption textarea 編集 → OK → /api/drive/files/update
// で comment 更新が round-trip する write-flow spec。
//
// drive.file.info.vue:182 の describe() は os.popupAsyncWithDialog で
// MkFileCaptionEditWindow を popup する。dialog 内には MkTextarea (autofocus)
// が 1 つあり、OK click で `done(caption)` callback → comment field を
// drive/files/update に流す (空文字なら null clear)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { uploadTinyPNG } from '../../../fixtures/files';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /my/drive/file/:fileId describe save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('describe button → caption textarea → OK → /api/drive/files/update', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 file を upload
    const file = await uploadTinyPNG(request, baseURL!, root.token, `pw-cap-${Date.now()}.png`);
    const fileId = file.id;

    // 2. detail page open
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/drive/file/${fileId}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // hydrate を file 名で確認
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      file.name,
      { timeout: 20_000 },
    );

    // 3. describe button (= kvEditBtn 系のうち、内部に "Description" 文字列を
    // 含む button) を click。drive.file.info.vue にはレイアウト上 2 つの
    // kvEditBtn (move folder / describe) があり、describe は MkKeyValue
    // の key text が "Description"。最初の hit で当てる。
    await clickWhenReady(page, '「Description」のボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          (b.textContent ?? '').includes('Description') &&
          b.querySelector('i.ti-pencil') !== null,
      ),
    );

    // 4. MkModalWindow + MkTextarea が出現するまで待つ。caption textarea は
    // autofocus 属性付き。テキスト入力は post_note.spec.ts と同じ native
    // value setter pattern。
    await page.waitForFunction(
      () => document.querySelectorAll('textarea').length >= 1,
      { timeout: 10_000 },
    );

    const caption = `pw-caption-${Date.now()}`;
    await page.evaluate((c) => {
      const tas = Array.from(document.querySelectorAll('textarea')) as HTMLTextAreaElement[];
      const target = tas[tas.length - 1];
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLTextAreaElement.prototype,
        'value',
      )?.set;
      setter?.call(target, c);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, caption);

    // 5. OK button → drive/files/update が走る。MkModalWindow.vue:15 の OK
    // button は `{{ i18n.ts.done }} <i class="ti ti-check">` で text は
    // i18n 依存 ("Done" / "完了" 等) だが ti-check icon は固定。/my/drive/file
    // 詳細 page には他の ti-check icon button が無いので構造ベースで安定。
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/drive/files/update') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, 'i.ti-check を持つボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) => !b.disabled && b.querySelector('i.ti-check') !== null,
      ),
    );
    await updateResp;

    // 6. API 経由で comment 反映 verify
    const showResp = await request.post(`${baseURL}/api/drive/files/show`, {
      ignoreHTTPSErrors: true,
      data: { i: root.token, fileId },
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.comment).toBe(caption);
  });
});
