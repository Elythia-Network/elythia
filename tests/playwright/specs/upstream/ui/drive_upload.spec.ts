/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ドライブ (/my/drive) に、メニューの「Upload」で選んだファイルと、ドラッグ&ドロップ
// したファイルを上げると、一覧に出て、API で読み直しても自分のドライブにあることを
// 確かめる (#4049)。
//
// 上げる要求は multipart なので postDataJSON では読めない。上がったファイル
// (drive/files/create の応答) を drive/files/show で読み直して、名前と置き場所
// (フォルダの外) を見る。ドロップは、ファイルを入れた DataTransfer を持つ drop の
// イベントをドライブの本体に送る (OS からのドラッグは Playwright では起こせない)。
// 一覧に残ることは、再読み込みの後も見る。

import { expect, type Page, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickWhenReady } from '../../../fixtures/ui_click';
import { countDriveItems, distinctPNG } from '../../../fixtures/ui_drive';
import { clickMenuItem, clickOnlyTestId } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

// 上げる確認のダイアログで「Upload」を押し、drive/files/create の応答 (上がったファイル) を返す
async function confirmUpload(page: Page): Promise<{ id: string; name: string; folderId: string | null }> {
  const created = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/drive/files/create', { timeout: 20_000 });
  // ダイアログが開いた直後は、閉じかけのドライブのメニューにも「Upload」の項目が残り、
  // ダイアログのボタンは上げる前の処理が終わるまで押せない。メニューの外の、押せる
  // ボタンを待って押す
  await clickWhenReady(page, '確認のダイアログの「Upload」', () => {
    const hits = Array.from(document.querySelectorAll('button'))
      .filter((b) => (b.textContent ?? '').trim() === 'Upload' && b.closest('[role="menu"]') === null);
    return hits.length === 1 && !hits[0].disabled ? hits[0] : null;
  });
  const resp = await created;
  expect(resp.status(), `drive/files/create failed: ${await resp.text()}`).toBe(200);
  const file = await resp.json();
  // 上げ終わると「Done」で閉じる
  await clickButtonByText(page, 'Done');
  return file;
}

test.describe('UI: drive upload', () => {
  test.setTimeout(90_000);

  test('a chosen file and a dropped file are uploaded and listed', coversRoutes('/my/drive'), async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwdrvup'));
    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, '/my/drive');

    // メニューの「Upload」から選ぶ
    const chosenName = `pw-chosen-${Date.now()}.png`;
    const chooser = page.waitForEvent('filechooser', { timeout: 15_000 });
    await clickOnlyTestId(page, 'drive-menu');
    await clickMenuItem(page, 'Upload');
    await (await chooser).setFiles({ name: chosenName, mimeType: 'image/png', buffer: distinctPNG() });
    const chosen = await confirmUpload(page);
    expect(chosen.name).toBe(chosenName);
    await expect.poll(() => countDriveItems(page, 'drive-file', chosenName), { timeout: 15_000 }).toBe(1);

    // ドライブの本体にドロップする
    const droppedName = `pw-dropped-${Date.now()}.png`;
    const transfer = await page.evaluateHandle(({ name, b64 }) => {
      const dt = new DataTransfer();
      const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
      dt.items.add(new File([bytes], name, { type: 'image/png' }));
      return dt;
    }, { name: droppedName, b64: distinctPNG().toString('base64') });
    await page.getByTestId('drive-main').dispatchEvent('drop', { dataTransfer: transfer });
    const dropped = await confirmUpload(page);
    expect(dropped.name).toBe(droppedName);
    await expect.poll(() => countDriveItems(page, 'drive-file', droppedName), { timeout: 15_000 }).toBe(1);

    // API で読み直す: どちらも自分のドライブの、フォルダの外にある
    for (const f of [chosen, dropped]) {
      const shown = await (await callApi(request, 'drive/files/show', { i: user.token, fileId: f.id })).json();
      expect(shown.name).toBe(f.name);
      expect(shown.folderId).toBeNull();
      expect(shown.userId).toBe(user.id);
    }
    const listed = ((await (await callApi(request, 'drive/files', { i: user.token, limit: 10 })).json()) as { id: string }[]).map((f) => f.id);
    expect(new Set(listed)).toEqual(new Set([chosen.id, dropped.id]));

    // 再読み込みの後も一覧に出る
    await gotoRoute(page, baseURL!, '/my/drive');
    await expect.poll(() => countDriveItems(page, 'drive-file', chosenName), { timeout: 15_000 }).toBe(1);
    expect(await countDriveItems(page, 'drive-file', droppedName)).toBe(1);
  });
});
