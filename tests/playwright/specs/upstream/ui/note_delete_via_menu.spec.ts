/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 自分の note を /notes/:id で開いて 3-dot menu (ti-dots) → menu の
// "Delete" item (ti-fw ti-trash) を click → confirm OK →
// /api/notes/delete が round-trip する write-flow spec。
//
// MkNote の menuButton は ti-dots icon を持つ footer button (line 157-158)。
// click すると os.popupMenu で menu items を popup する (line 620)。menu
// items は MkMenu.vue で `<button class="_button">` + `<i class="ti-fw ...">`
// の組み合わせで render される。
//
// note の footer 自体には他にも button があるが、icon の class は ti-fw を
// 持たない。menu item の icon は ti-fw 修飾を持つので、`i.ti-fw.ti-trash`
// で menu の Delete だけを精度高く特定できる。
//
// popup menu 系 spec の最初の例。confirm dialog 経由で API 呼出は
// drive_file_delete などと同 pattern。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { NOT_FOUND_STATUS } from '../../../fixtures/backend';
import { clickButtonWithIcon, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: note 3-dot menu delete flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('open menu → click Delete → confirm OK → /api/notes/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 note を API で create
    const noteText = `pw-note-del-${Date.now()}`;
    const createResp = await callApi(request, 'notes/create', {
      i: root.token,
      text: noteText,
      visibility: 'home',
    });
    expect(createResp.status()).toBe(200);
    const noteId = (await createResp.json()).createdNote.id;
    expect(noteId).toBeTruthy();

    // 2. note 詳細ページを開く
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/notes/${noteId}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // note text が body に出るまで待つ
    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      noteText,
      { timeout: 20_000 },
    );

    // 3. 3-dot menu button (= ti-dots icon を持つ footer button) を click。
    // MkNote.vue:157 は `@mousedown.prevent="showMenu()"` で mousedown event
    // のみで popup を起動する (= click だけだと一切反応しない)。HTMLElement
    // の `.click()` は click event しか発火しないため、ここでは mousedown を
    // 明示 dispatch する。footer の他 button (renote / clip / renoteTime) も
    // 同 pattern で listener が mousedown のため、ti-* footer button への
    // 操作は全て mousedown dispatch を使う規約。
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some((b) => b.querySelector('i.ti-dots') !== null);
      },
      { timeout: 15_000 },
    );
    await page.evaluate(() => {
      const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
      const target = btns.find((b) => b.querySelector('i.ti-dots') !== null);
      target?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }));
    });

    // 4. popup menu が DOM に現れる。menu item は <i class="ti-fw ti-XXX">
    // を持つ。"Delete" item は ti-fw + ti-trash + danger style で唯一。
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some((b) => b.querySelector('i.ti-fw.ti-trash') !== null);
      },
      { timeout: 10_000 },
    );

    // 5. Delete menu item を click → confirm dialog 出現
    await clickButtonWithIcon(page, 'i.ti-fw.ti-trash');

    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    // 6. confirm OK → /api/notes/delete
    const deleteResp = page.waitForResponse(
      (r) => r.url().includes('/api/notes/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await deleteResp;

    // 7. API 経由で削除確認 — notes/show は 404 + NO_SUCH_NOTE を返す
    // (apierr.NoSuchNote() = handler.go:357、UUID 24fcbfc6-2e37-42b6-8388-c29b3861a08d)
    const showResp = await callApi(request, 'notes/show', {
      i: root.token,
      noteId,
    });
    expect(showResp.status()).toBe(NOT_FOUND_STATUS);
    const showBody = await showResp.json();
    expect(showBody.error?.code).toBe('NO_SUCH_NOTE');
    expect(showBody.error?.id).toBe('24fcbfc6-2e37-42b6-8388-c29b3861a08d');
  });
});
