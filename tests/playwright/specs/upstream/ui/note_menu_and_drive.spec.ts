/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ノートのメニューの「Copy link」と「Embed」、ドライブの複数選択とまとめての移動、アバターの
// 画像のクロップを画面で通す (#4072)。
//
//   Copy link  クリップボードにノートの URL が入る (クリップボードを読む権限を与えて読む)
//   Embed      埋め込みのコードを作るダイアログで、/embed/notes/<id> を指す iframe のコードができる
//   ドライブ   メニューの「Edit」で選ぶモードにし、2 つのファイルを選んで「Move...」でフォルダへ移す
//   クロップ   プロフィールのアバターを端末の画像から変え、「Crop」を選んで正方形に切り抜いて上げる。
//              横長の画像を渡し、上がったファイルが正方形になることで見る

import { expect, type Page, test } from '@playwright/test';
import { signinWithState } from '../../../fixtures/settings_table';
import { clickButtonByText, clickWhenReady } from '../../../fixtures/ui_click';
import { clickDriveItem, distinctPNG, uploadPNG } from '../../../fixtures/ui_drive';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, clickMenuItem, freshUser, postNote } from '../../../fixtures/ui_timeline';

/** A PNG of `width` x `height`, drawn in the page. */
async function pngOfSize(page: Page, width: number, height: number): Promise<Buffer> {
  const dataUrl = await page.evaluate(([w, h]) => {
    const c = document.createElement('canvas');
    c.width = w;
    c.height = h;
    const ctx = c.getContext('2d')!;
    ctx.fillStyle = '#3366cc';
    ctx.fillRect(0, 0, w, h);
    return c.toDataURL('image/png');
  }, [width, height]);
  return Buffer.from(dataUrl.split(',')[1], 'base64');
}

test.describe('UI: note menu and drive', () => {
  test.setTimeout(90_000);

  test('copy the link of a note', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwcpl');
    const noteId = await postNote(request, me, `pw copy ${Date.now().toString(36)}`);
    await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, `/notes/${noteId}`);
    await page.getByTestId('note-menu').first().click();
    await clickMenuItem(page, 'Copy link');
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText()), { message: 'ノートの URL がクリップボードに入らない' }).toBe(`${baseURL}/notes/${noteId}`);
  });

  test('generate the embed code of a note', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwemb');
    const noteId = await postNote(request, me, `pw embed ${Date.now().toString(36)}`);
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, `/notes/${noteId}`);
    await page.getByTestId('note-menu').first().click();
    await clickMenuItem(page, 'Embed');
    // プレビューが整うまで押せない
    await clickWhenReady(page, '「Generate embed code」', () => {
      const b = Array.from(document.querySelectorAll('button')).find((x) => (x.textContent ?? '').trim() === 'Generate embed code');
      return b && !b.disabled ? b : null;
    });
    await expect(page.getByText(/<iframe/).first(), '埋め込みのコードが出ない').toBeVisible({ timeout: 15_000 });
    await expect(page.getByText(new RegExp(`/embed/notes/${noteId}`)).first(), 'コードがノートを指さない').toBeVisible();
  });

  test('select two files and move them to a folder', coversRoutes('/my/drive'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwdrv');
    const folderName = `pwmove${Date.now().toString(36)}`;
    const folder = await apiOk(request, 'drive/folders/create', { i: me.token, name: folderName }) as { id: string };
    // 同じ中身を上げると同じファイルにまとめられるので、中身を変える
    const f1 = await uploadPNG(request, baseURL!, me.token, 'pw-move-1.png', distinctPNG());
    const f2 = await uploadPNG(request, baseURL!, me.token, 'pw-move-2.png', distinctPNG());
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/my/drive');

    await page.getByTestId('drive-menu').click();
    await page.locator('[role="menu"] [role^="menuitem"]').filter({ hasText: /^\s*Edit\s*$/ }).click();
    await page.keyboard.press('Escape');
    await clickDriveItem(page, 'drive-file', 'pw-move-1.png');
    await clickDriveItem(page, 'drive-file', 'pw-move-2.png');
    await clickButtonByText(page, 'Move...');

    // フォルダの選択のダイアログも中にドライブを描く。ダイアログは後から開くので、文書の最後の
    // 同じ名前のフォルダがダイアログのもの。カードの選択のボタンで選び、印 (ti-check) が付いてから進む。
    // 中へ入って「Select a folder」で選ぶ道は、入りきる前に押すと上の階層を選んでしまうので使わない
    const card = page.locator('[data-testid="drive-folder"]').filter({ hasText: folderName }).last();
    await clickWhenReady(page, `ダイアログのフォルダ「${folderName}」の選択のボタン`, (n: string) => Array.from(document.querySelectorAll('[data-testid="drive-folder"]')).filter((el) => (el.textContent ?? '').includes(n)).at(-1)?.querySelector('button'), folderName);
    await expect(card.locator('.ti-check'), 'フォルダを選べない').toHaveCount(1);
    const moved = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/drive/files/move-bulk', { timeout: 15_000 });
    // 後から開いたダイアログの「Done」(文書の最後のもの) が押せるようになってから押す
    await clickWhenReady(page, 'フォルダの選択の「Done」', () => {
      const b = Array.from(document.querySelectorAll('[data-testid="modal-window-ok"]')).at(-1) as HTMLButtonElement | undefined;
      return b && !b.disabled ? b : null;
    });
    expect((await moved).ok(), 'drive/files/move-bulk が失敗した').toBe(true);
    for (const f of [f1, f2]) {
      const shown = await apiOk(request, 'drive/files/show', { i: me.token, fileId: f.id }) as { folderId: string | null };
      expect(shown.folderId, '選んだファイルがフォルダへ移らない').toBe(folder.id);
    }
  });

  test('crop an avatar to a square', coversRoutes('/settings/profile'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwcrop');
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/settings/profile');
    const wide = await pngOfSize(page, 120, 60);

    await clickButtonByText(page, 'Change avatar');
    const chooser = page.waitForEvent('filechooser', { timeout: 15_000 });
    await clickMenuItem(page, 'Upload');
    await (await chooser).setFiles({ name: 'pw-wide.png', mimeType: 'image/png', buffer: wide });
    // 切り抜くかを尋ねる確認で「Crop」、切り抜きのダイアログで OK
    await page.getByTestId('modal-dialog-ok').click();
    await expect(page.getByText('Crop image').first(), '切り抜きのダイアログが出ない').toBeVisible({ timeout: 15_000 });
    await page.getByTestId('modal-window-ok').click();

    const created = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/drive/files/create', { timeout: 20_000 });
    // アップローダーの「Upload」(メニューの項目ではなく、押せるもの)
    await clickWhenReady(page, 'アップローダーの「Upload」', () => {
      const hits = Array.from(document.querySelectorAll('button')).filter((b) => (b.textContent ?? '').trim() === 'Upload' && b.closest('[role="menu"]') === null);
      return hits.length === 1 && !hits[0].disabled ? hits[0] : null;
    });
    const file = await (await created).json() as { id: string; properties: { width?: number; height?: number } };
    expect(file.properties.width, '切り抜かれていない').toBe(file.properties.height);
    expect(file.properties.width, '切り抜かれていない').toBeLessThan(120);
  });
});
