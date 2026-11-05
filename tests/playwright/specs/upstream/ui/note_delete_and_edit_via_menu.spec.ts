/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 自分のノートの「...」のメニューから「Delete and edit」を選ぶと、ノートが消え、
// 本文と CW が投稿フォームに戻ることを確かめる (#4043)。
//
// 消えたことは notes/delete に送った noteId と、notes/show が見つからなくなることで
// 見る。戻った本文はそのまま送れることまで確かめる。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { NOT_FOUND_STATUS } from '../../../fixtures/backend';
import { createNote, showNoteRaw } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId } from '../../../fixtures/ui_click';
import {
  clickMenuItem,
  mousedownOnlyTestId,
  postTextValue,
  submitPostForm,
  waitForApiBody,
  waitForPostForm,
} from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: note delete and edit via menu', () => {
  test.setTimeout(90_000);

  test('Delete and edit removes the note and puts its text back in the form', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwredraft'));
    const text = `pw-redraft-${Date.now()}`;
    const cw = `pw-redraft-cw-${Date.now()}`;
    const note = await createNote(request, user.token, { text, cw });
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/notes/${note.id}`);
    await expect(page.getByText(cw, { exact: true })).toBeVisible({ timeout: 20_000 });
    await mousedownOnlyTestId(page, 'note-menu');
    await clickMenuItem(page, 'Delete and edit');
    const deleted = waitForApiBody(page, 'notes/delete');
    await clickByTestId(page, 'modal-dialog-ok');
    expect((await deleted).noteId).toBe(note.id);

    await waitForPostForm(page);
    expect(await postTextValue(page)).toBe(text);
    await expect(page.getByTestId('post-form-cw')).toHaveValue(cw);
    await expect.poll(async () => (await showNoteRaw(request, user.token, note.id)).status()).toBe(NOT_FOUND_STATUS);

    const sent = await submitPostForm(page);
    expect(sent.text).toBe(text);
    expect(sent.cw).toBe(cw);
  });
});
