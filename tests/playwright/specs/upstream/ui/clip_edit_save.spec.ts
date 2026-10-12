/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /clips/:id ページの header pencil button → MkFormDialog で name 編集 →
// OK で /api/clips/update が走ることを verify する write-flow spec。
//
// clip.vue の headerActions は MkPageHeader headerActions = `ti-pencil`
// アイコンの button のみ (own clip + signed in 時)。click すると
// os.form() で MkFormDialog が popup し、name / description / isPublic
// の 3 fields が並ぶ。最初の text input が name なのでそこを書き換えて
// MkFormDialog の OK button を click すれば update round-trip が走る。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /clips/:id edit name round-trip', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('header pencil → MkFormDialog rename → /api/clips/update', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 clip を API で create
    const initialName = `pw-clip-${Date.now()}`;
    const createResp = await request.post(`${baseURL}/api/clips/create`, {
      ignoreHTTPSErrors: true,
      data: { i: root.token, name: initialName, isPublic: false },
    });
    expect(createResp.status()).toBe(200);
    const clip = await createResp.json();
    const clipId: string = clip.id;
    expect(clipId).toBeTruthy();

    // 2. clip 詳細ページを開いて hydrate を待つ
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/clips/${clipId}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      initialName,
      { timeout: 20_000 },
    );

    // 3. header の pencil アイコン button (= edit handler) が hydrate
    // するまで待ってから click。
    //
    // 単に「最初の ti-pencil button」を取ると **navbar の "Note" (投稿)
    // button** に当たる (こちらも ti-pencil を持ち DOM 上で先に来る)。
    // 実際 post form が開いてしまい clips/update は永久に飛ばなかった。
    // header action の pencil は icon only (= textContent が空) なので、
    // "Note" / "Edit widgets" 等ラベル付きの pencil button を除外する。
    await clickWhenReady(page, 'i.ti-pencil を持つボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) => b.querySelector('i.ti-pencil') !== null && (b.textContent ?? '').trim() === '',
      ),
    );

    // 4. MkFormDialog が popup → 最初の text input (= name) が見える
    await page.waitForFunction(
      () => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.filter((i) => i.type === 'text').length >= 1;
      },
      { timeout: 10_000 },
    );

    // 5. 新しい name を投入。MkFormDialog 内の text input は name で、
    // settings layout の MkSuperMenu search 入力 (type=search) と被らない
    // よう type=text のみ抽出。dialog mount 直後の最初 type=text が name。
    const newName = `pw-clip-renamed-${Date.now()}`;
    await page.evaluate((name) => {
      const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
      const textInputs = inputs.filter((i) => i.type === 'text');
      const target = textInputs[textInputs.length - 1];
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, name);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, newName);

    // 6. MkFormDialog OK → clips/update round-trip
    // 注: `[data-testid="modal-dialog-ok"]` 属性は MkDialog.vue (= os.confirm /
    // os.alert 系) にしかない。MkFormDialog.vue (= os.form 経由) は
    // MkModalWindow の `<MkButton primary gradate small rounded>{{
    // i18n.ts.done }} <i class="ti ti-check">` (MkModalWindow.vue:15) を
    // OK にしており data-cy 無し。"Done" text + ti-check icon で識別する。
    // 注: `"Done"` は `i18n.ts.done` = en-US value (en-US.yml: `done: "Done"`)。
    // playwright.config.ts の `locale: 'en-US'` 強制設定に依存。
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/clips/update') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '「Done」のボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          !b.disabled &&
          b.querySelector('i.ti-check') !== null &&
          (b.textContent ?? '').includes('Done'),
      ),
    );
    const update = await updateResp;
    expect(update.status()).toBeLessThan(300);

    // 7. API GET で round-trip verify
    const showResp = await request.post(`${baseURL}/api/clips/show`, {
      ignoreHTTPSErrors: true,
      data: { i: root.token, clipId },
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.id).toBe(clipId);
    expect(shown.name).toBe(newName);
  });
});
