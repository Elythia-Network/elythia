/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ノートのリノートのメニューから「Quote」を選ぶと、投稿フォームが引用の状態で開き、
// 送ると引用先の `renoteId` と本文が送られることを確かめる (#4043)。
//
// 引用の状態は、フォームに引用先のノートが出ていること (MkNoteSimple) と、
// 送られた renoteId の両方で見る。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import {
  clickMenuItem,
  mousedownOnlyTestId,
  setPostText,
  submitPostForm,
  waitForPostForm,
} from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: quote a note via the renote menu', () => {
  test.setTimeout(90_000);

  test('Quote opens the form quoting the note and sends renoteId with text', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwquoteto'));
    const user = await signupUser(request, randomUsername('pwquote'));
    const target = await createNote(request, author.token, { text: `pw-quote-target-${Date.now()}` });
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/notes/${target.id}`);
    await expect(page.getByText(target.text!, { exact: true })).toBeVisible({ timeout: 20_000 });
    await mousedownOnlyTestId(page, 'note-renote');
    await clickMenuItem(page, 'Quote');
    await waitForPostForm(page);

    // フォームの中に引用先の本文が出ている (ページの本文と合わせて 2 つになる)
    await expect(page.getByText(target.text!, { exact: true })).toHaveCount(2);

    const text = `pw-quote-${Date.now()}`;
    await setPostText(page, text);
    const sent = await submitPostForm(page);
    expect(sent.renoteId).toBe(target.id);
    expect(sent.text).toBe(text);

    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(notes).toHaveLength(1);
    expect(notes[0].renoteId).toBe(target.id);
    expect(notes[0].text).toBe(text);
  });
});
