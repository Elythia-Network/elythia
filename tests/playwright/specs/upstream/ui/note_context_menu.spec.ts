/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ノートを右クリックすると、そのノートのメニューが出ることを確かめる (#4043)。
//
// メニューが出ただけでは、どのノートのメニューか分からない。項目
// (「Add to favorites」) を選び、右クリックしたノートがお気に入りに入ることで確かめる。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickMenuItem, waitForApiBody } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: note context menu', () => {
  test.setTimeout(90_000);

  test('right-clicking a note opens its menu', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwctxto'));
    const user = await signupUser(request, randomUsername('pwctx'));
    const note = await createNote(request, author.token, { text: `pw-ctx-${Date.now()}` });
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/notes/${note.id}`);
    const body = page.getByText(note.text!, { exact: true });
    await expect(body).toBeVisible({ timeout: 20_000 });
    await expect(page.locator('[role="menu"]')).toHaveCount(0);
    await body.click({ button: 'right' });
    await expect(page.locator('[role="menu"]')).toHaveCount(1, { timeout: 10_000 });

    const created = waitForApiBody(page, 'notes/favorites/create');
    await clickMenuItem(page, 'Add to favorites');
    expect((await created).noteId).toBe(note.id);
    await expect
      .poll(async () => ((await (await callApi(request, 'i/favorites', { i: user.token })).json()) as { noteId: string }[]).map((f) => f.noteId))
      .toEqual([note.id]);
  });
});
