/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ファイル選択のダイアログ (MkDriveFileSelectDialog) を、投稿フォームとプロフィールの
// アイコンから開いて選ぶと、選んだファイルが使われることを確かめる (#4049)。
//
// ドライブに 2 つ置き、2 つ目を選ぶ (1 つしか無いと、「先頭のものを使う」回帰を
// 見逃す)。使われたことは、送った値 (notes/create の fileIds、i/update の avatarId) と、
// API で読み直した値で見る。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText } from '../../../fixtures/ui_click';
import { clickDriveItem, distinctPNG, uploadPNG } from '../../../fixtures/ui_drive';
import {
  clickMenuItem,
  clickOnlyTestId,
  openPostForm,
  setPostText,
  submitPostForm,
  waitForApiBody,
} from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: drive file select dialog', () => {
  test.setTimeout(90_000);

  test('the post form attaches the file chosen in the dialog', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwselnote'));
    const stamp = Date.now();
    await uploadPNG(request, baseURL!, user.token, `pw-sel-a-${stamp}.png`, distinctPNG());
    const target = await uploadPNG(request, baseURL!, user.token, `pw-sel-b-${stamp}.png`, distinctPNG());
    await uiSigninAs(page, request, baseURL!, user);

    await openPostForm(page);
    await setPostText(page, `pw-select-${stamp}`);
    await clickOnlyTestId(page, 'post-form-attach-drive');
    await clickDriveItem(page, 'drive-file', target.name);
    await clickOnlyTestId(page, 'modal-window-ok');
    const sent = await submitPostForm(page);
    expect(sent.fileIds).toEqual([target.id]);

    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(notes).toHaveLength(1);
    expect(notes[0].fileIds).toEqual([target.id]);
  });

  test('the profile avatar uses the file chosen in the dialog', coversRoutes('/settings/profile'), async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwselava'));
    const stamp = Date.now();
    await uploadPNG(request, baseURL!, user.token, `pw-ava-a-${stamp}.png`, distinctPNG());
    const target = await uploadPNG(request, baseURL!, user.token, `pw-ava-b-${stamp}.png`, distinctPNG());
    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, '/settings/profile');

    await clickButtonByText(page, 'Change avatar');
    await clickMenuItem(page, 'From Drive');
    await clickDriveItem(page, 'drive-file', target.name);
    const sent = waitForApiBody(page, 'i/update');
    await clickOnlyTestId(page, 'modal-window-ok');
    expect((await sent).avatarId).toBe(target.id);

    await expect.poll(async () => (await (await callApi(request, 'i', { i: user.token })).json()).avatarId, { timeout: 15_000 }).toBe(target.id);
  });
});
