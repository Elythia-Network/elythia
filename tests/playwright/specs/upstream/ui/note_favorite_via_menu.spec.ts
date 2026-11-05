/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ノートの「...」のメニューから「Add to favorites」を選ぶと、お気に入りに入り、
// 再読み込みの後のメニューが「Remove from favorites」になり、それで外せることを
// 確かめる (#4043)。入ったこと・外れたことは i/favorites で読み直す。

import { expect, test, type APIRequestContext } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type Principal, randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickMenuItem, mousedownOnlyTestId, waitForApiBody } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

async function favoriteNoteIds(request: APIRequestContext, user: Principal): Promise<string[]> {
  const resp = await callApi(request, 'i/favorites', { i: user.token });
  expect(resp.status()).toBe(200);
  return ((await resp.json()) as { noteId: string }[]).map((f) => f.noteId);
}

test.describe('UI: note favorite via menu', () => {
  test.setTimeout(90_000);

  test('Add to favorites, then Remove from favorites after a reload', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwfavto'));
    const user = await signupUser(request, randomUsername('pwfav'));
    const note = await createNote(request, author.token, { text: `pw-fav-${Date.now()}` });
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/notes/${note.id}`);
    await expect(page.getByText(note.text!, { exact: true })).toBeVisible({ timeout: 20_000 });
    await mousedownOnlyTestId(page, 'note-menu');
    const created = waitForApiBody(page, 'notes/favorites/create');
    await clickMenuItem(page, 'Add to favorites');
    expect((await created).noteId).toBe(note.id);
    await expect.poll(() => favoriteNoteIds(request, user)).toEqual([note.id]);

    await gotoRoute(page, baseURL!, `/notes/${note.id}`);
    await expect(page.getByText(note.text!, { exact: true })).toBeVisible({ timeout: 20_000 });
    await mousedownOnlyTestId(page, 'note-menu');
    const deleted = waitForApiBody(page, 'notes/favorites/delete');
    await clickMenuItem(page, 'Remove from favorites');
    expect((await deleted).noteId).toBe(note.id);
    await expect.poll(() => favoriteNoteIds(request, user)).toEqual([]);
  });
});
