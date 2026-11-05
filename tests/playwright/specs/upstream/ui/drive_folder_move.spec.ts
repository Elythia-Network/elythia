/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ドライブでメニューからフォルダを作り、ファイルをそのフォルダへドラッグして移すと、
// 送った値 (drive/folders/create と drive/files/move-bulk) のとおりに、API で
// 読み直してもファイルがそのフォルダにあることを確かめる (#4049)。
//
// 移した後は、フォルダを開くとファイルがあり、ドライブの最上位には無いことも見る。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId } from '../../../fixtures/ui_click';
import { clickDriveItem, countDriveItems, distinctPNG, uploadPNG } from '../../../fixtures/ui_drive';
import { clickMenuItem, clickOnlyTestId, waitForApiBody } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: drive folder create and move', () => {
  test.setTimeout(90_000);

  test('create a folder from the menu and drag a file into it', coversRoutes('/my/drive'), async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwdrvmv'));
    const fileName = `pw-move-${Date.now()}.png`;
    const file = await uploadPNG(request, baseURL!, user.token, fileName, distinctPNG());
    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, '/my/drive');
    await expect.poll(() => countDriveItems(page, 'drive-file', fileName), { timeout: 15_000 }).toBe(1);

    // メニューからフォルダを作る
    const folderName = `pw-folder-${Date.now()}`;
    await clickOnlyTestId(page, 'drive-menu');
    await clickMenuItem(page, 'Create a folder');
    const nameInput = page.locator('input[placeholder="Folder name"]');
    await expect(nameInput).toHaveCount(1, { timeout: 10_000 });
    await nameInput.fill(folderName);
    const createdBody = waitForApiBody(page, 'drive/folders/create');
    const createdResp = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/drive/folders/create', { timeout: 15_000 });
    await clickByTestId(page, 'modal-dialog-ok');
    expect((await createdBody).name).toBe(folderName);
    const folder = await (await createdResp).json() as { id: string };
    await expect.poll(() => countDriveItems(page, 'drive-folder', folderName), { timeout: 15_000 }).toBe(1);

    // ファイルをフォルダへドラッグする
    const moved = waitForApiBody(page, 'drive/files/move-bulk');
    const fileEl = page.getByTestId('drive-file').filter({ hasText: fileName.replace(/\.png$/, '') });
    const folderEl = page.getByTestId('drive-folder').filter({ hasText: folderName });
    await fileEl.dragTo(folderEl);
    expect(await moved).toMatchObject({ fileIds: [file.id], folderId: folder.id });

    await expect.poll(async () => (await (await callApi(request, 'drive/files/show', { i: user.token, fileId: file.id })).json()).folderId, { timeout: 15_000 }).toBe(folder.id);
    const inFolder = ((await (await callApi(request, 'drive/files', { i: user.token, folderId: folder.id })).json()) as { id: string }[]).map((f) => f.id);
    expect(inFolder).toEqual([file.id]);

    // 開き直すと、最上位には無く、フォルダを開くとある
    await gotoRoute(page, baseURL!, '/my/drive');
    await expect.poll(() => countDriveItems(page, 'drive-folder', folderName), { timeout: 15_000 }).toBe(1);
    expect(await countDriveItems(page, 'drive-file', fileName)).toBe(0);
    await clickDriveItem(page, 'drive-folder', folderName);
    await expect.poll(() => countDriveItems(page, 'drive-file', fileName), { timeout: 15_000 }).toBe(1);
  });
});
