/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 確認のダイアログ (MkDialog) を、キャンセル・Esc・背景のクリックで閉じたときは、
// 確認の後の API が呼ばれないことを確かめる (#4049)。最後に OK を押すと呼ばれる
// (同じ手順で、見張りが API を拾えることの確かめ)。
//
// 題材はノートの削除 (「...」のメニューの「Delete」)。呼ばれないことは待てないので、
// ダイアログが閉じてから 1 秒の間、notes/delete の要求を見張る。ノートが残っている
// ことは notes/show でも見る。

import { expect, type Page, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote, showNoteRaw } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId } from '../../../fixtures/ui_click';
import { clickMenuItem, mousedownOnlyTestId } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

const WATCH_MS = 1_000;

// 削除の確認のダイアログを開く
async function openDeleteConfirm(page: Page): Promise<void> {
  await mousedownOnlyTestId(page, 'note-menu');
  await clickMenuItem(page, 'Delete');
  await expect(page.getByTestId('modal-dialog-ok')).toHaveCount(1, { timeout: 10_000 });
  await expect(page.getByTestId('modal-dialog-ok')).toBeVisible();
}

test.describe('UI: confirm dialog dismissal', () => {
  test.setTimeout(90_000);

  test('cancel, Esc, and a background click do not call the API; OK does', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwconfirm'));
    const note = await createNote(request, user.token, { text: `pw-confirm-${Date.now()}` });
    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, `/notes/${note.id}`);
    await expect(page.getByText(note.text!, { exact: true })).toBeVisible({ timeout: 20_000 });

    const deletes: unknown[] = [];
    page.on('request', (r) => {
      if (r.method() === 'POST' && new URL(r.url()).pathname === '/api/notes/delete') deletes.push(r.postDataJSON());
    });

    const dismissals: [string, () => Promise<void>][] = [
      ['cancel', () => clickByTestId(page, 'modal-dialog-cancel')],
      ['Esc', () => page.keyboard.press('Escape')],
      // 背景はダイアログの外側の、ダイアログが重ならない位置 (左上の隅) を押す
      ['background', () => page.getByTestId('bg').last().click({ position: { x: 5, y: 5 } })],
    ];
    for (const [how, dismiss] of dismissals) {
      await openDeleteConfirm(page);
      await dismiss();
      await expect(page.getByTestId('modal-dialog-ok'), `${how} でダイアログが閉じない`).toHaveCount(0, { timeout: 10_000 });
      await page.waitForTimeout(WATCH_MS);
      expect(deletes, `${how} で notes/delete が呼ばれた`).toEqual([]);
      expect((await showNoteRaw(request, user.token, note.id)).status(), `${how} でノートが消えた`).toBe(200);
    }

    // OK では呼ばれる
    await openDeleteConfirm(page);
    await clickByTestId(page, 'modal-dialog-ok');
    await expect.poll(() => deletes.length, { timeout: 10_000 }).toBe(1);
    expect(deletes[0]).toMatchObject({ noteId: note.id });
    await expect.poll(async () => (await showNoteRaw(request, user.token, note.id)).status()).not.toBe(200);
  });
});
