/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ノートの「...」のメニューの「Clip」から自分のクリップを選ぶと、そのクリップに
// ノートが入ることを確かめる (#4043)。入ったことは clips/notes で読み直し、
// 再読み込みの後のメニューでクリップの件数が 1 になっていることも見る。
//
// メニューのクリップの名前には件数と上限が付く (get-note-menu.ts の getClipName)。
// 上限は role の policy (noteEachClipsLimit) なので i から読む。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickMenuItem, mousedownOnlyTestId, openSubmenu, waitForApiBody, waitForMenuItem } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: note clip via menu', () => {
  test.setTimeout(90_000);

  test('choosing a clip from the Clip menu adds the note to it', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwclipto'));
    const user = await signupUser(request, randomUsername('pwclip'));
    const note = await createNote(request, author.token, { text: `pw-clip-${Date.now()}` });
    const clipName = `pw-clip-${Date.now()}`;
    const clipResp = await callApi(request, 'clips/create', { i: user.token, name: clipName });
    expect(clipResp.status()).toBe(200);
    const clip = await clipResp.json();
    const me = await (await callApi(request, 'i', { i: user.token })).json();
    const limit = me.policies.noteEachClipsLimit as number;

    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, `/notes/${note.id}`);
    await expect(page.getByText(note.text!, { exact: true })).toBeVisible({ timeout: 20_000 });
    await mousedownOnlyTestId(page, 'note-menu');
    await openSubmenu(page, 'Clip');
    const added = waitForApiBody(page, 'clips/add-note');
    await clickMenuItem(page, `${clipName} (0/${limit})`);
    expect(await added).toMatchObject({ clipId: clip.id, noteId: note.id });

    await expect
      .poll(async () => {
        const notes = await (await callApi(request, 'clips/notes', { i: user.token, clipId: clip.id })).json();
        return (notes as { id: string }[]).map((n) => n.id);
      })
      .toEqual([note.id]);

    await gotoRoute(page, baseURL!, `/notes/${note.id}`);
    await expect(page.getByText(note.text!, { exact: true })).toBeVisible({ timeout: 20_000 });
    await mousedownOnlyTestId(page, 'note-menu');
    await openSubmenu(page, 'Clip');
    await waitForMenuItem(page, `${clipName} (1/${limit})`);
  });
});
