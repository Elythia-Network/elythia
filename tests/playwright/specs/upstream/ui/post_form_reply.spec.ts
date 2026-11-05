/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ノートの返信ボタンから投稿フォームを開いて送ると、返信先の `replyId` と本文が
// 送られることを確かめる (#4043)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickOnlyTestId, setPostText, submitPostForm, waitForPostForm } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: reply from a note', () => {
  test.setTimeout(90_000);

  test('the reply button opens the form and sends replyId', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwreplyto'));
    const user = await signupUser(request, randomUsername('pwreply'));
    const target = await createNote(request, author.token, { text: `pw-reply-target-${Date.now()}` });
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/notes/${target.id}`);
    await expect(page.getByText(target.text!, { exact: true })).toBeVisible({ timeout: 20_000 });
    await clickOnlyTestId(page, 'note-reply');
    await waitForPostForm(page);

    const text = `pw-reply-${Date.now()}`;
    await setPostText(page, text);
    const sent = await submitPostForm(page);
    expect(sent.replyId).toBe(target.id);
    expect(sent.text).toBe(text);

    // users/notes は既定で返信を含めない
    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id, withReplies: true })).json();
    expect(notes).toHaveLength(1);
    expect(notes[0].replyId).toBe(target.id);
  });
});
