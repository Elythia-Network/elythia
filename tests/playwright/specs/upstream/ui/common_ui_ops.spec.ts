/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 共通の UI の操作 (#4072)。
//
//   入力ダイアログ  テーマエディタの「Save as...」の名前の入力は 1 文字以上が要る。空のままでは OK を
//                   押せず、名前を入れて保存すると、入れたテーマが端末の設定 (themes) に入る
//   メニュー        ドライブのメニューを矢印キーで「Create a folder」まで動かし、Enter で選ぶと、
//                   フォルダの名前を尋ねるダイアログを経てフォルダが作られる
//   ウィンドウ      リンクの右クリックの「Open in window」でページのウィンドウを開き、最大化して元に戻し、
//                   閉じる
//
// ウィンドウの見出しのボタンは文字を持たないので、アイコン (ti-rectangle / ti-picture-in-picture / ti-x)
// で取る。最大化はクラス (ハッシュ化される) しか変わらないので、ボタンが「元に戻す」に変わることと、
// ウィンドウの根 (position: fixed) が、前は画面より小さく後は画面いっぱいになることで見る。

import { expect, test } from '@playwright/test';
import { readPref, signinWithState } from '../../../fixtures/settings_table';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, clickMenuItem, freshUser } from '../../../fixtures/ui_timeline';

test.describe('UI: common dialogs, menus and windows', () => {
  test.setTimeout(60_000);

  test('the name of a theme is required, and saving installs it', coversRoutes('/theme-editor'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwthe');
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/theme-editor');
    // 「Save as...」は見出しの右のアイコン (ti-check) のボタンとして出る
    await clickButtonWithIcon(page, 'i.ti-check');

    const ok = page.getByTestId('modal-dialog-ok');
    const input = page.getByTestId('modal-dialog-input').locator('input');
    await expect(input).toHaveValue('');
    await expect(ok, '空の名前で OK を押せる').toBeDisabled();
    await input.press('Enter');
    await expect(ok, 'Enter で空の名前のまま進んだ').toBeDisabled();

    const name = `PW editor ${Date.now().toString(36)}`;
    await input.fill(name);
    await expect(ok).toBeEnabled();
    // 名前があれば Enter で送れる (空のときに Enter で進まなかったことの対照)
    // 送った後は「インストールした」の知らせのダイアログ (OK を持つ) が出るので、入力欄が消えたことで見る
    await input.press('Enter');
    await expect(page.getByTestId('modal-dialog-input'), '名前を入れても Enter で送れない').toHaveCount(0);
    await expect.poll(async () => ((await readPref(page, 'themes')) as { name: string }[] | undefined ?? []).map((t) => t.name), { message: '保存したテーマが入らない' }).toContain(name);
  });

  test('move through a menu with the arrow keys and choose with Enter', coversRoutes('/my/drive'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwkey');
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/my/drive');
    await page.getByTestId('drive-menu').click();
    await expect(page.locator('[role="menu"]')).toHaveCount(1);

    const focused = () => page.evaluate(() => (document.activeElement?.textContent ?? '').trim());
    let reached = false;
    for (let n = 0; n < 20 && !reached; n++) {
      await page.keyboard.press('ArrowDown');
      reached = (await focused()) === 'Create a folder';
    }
    expect(reached, '矢印キーで「Create a folder」まで動けない').toBe(true);
    await page.keyboard.press('Enter');

    const name = `pwkey${Date.now().toString(36)}`;
    await page.getByTestId('modal-dialog-input').locator('input').fill(name);
    const created = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/drive/folders/create', { timeout: 15_000 });
    await page.getByTestId('modal-dialog-ok').click();
    expect((await created).ok(), 'drive/folders/create が失敗した').toBe(true);
    const folders = await apiOk(request, 'drive/folders', { i: me.token, limit: 100 }) as { name: string }[];
    expect(folders.map((f) => f.name), 'Enter で選んだ操作でフォルダが作られない').toContain(name);
  });

  test('open a page in a window, maximize it, restore it and close it', coversRoutes('/my/drive'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwwin');
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/');
    // ナビの「Drive」へのリンクを右クリックして、ウィンドウで開く
    await page.locator('a[href="/my/drive"]').first().click({ button: 'right' });
    await clickMenuItem(page, 'Open in window');

    const maximize = page.locator('button:has(i.ti-rectangle)');
    const restore = page.locator('button:has(i.ti-picture-in-picture)');
    await expect(maximize, 'ウィンドウが開かない').toHaveCount(1, { timeout: 15_000 });
    await expect(page.getByTestId('drive-main').last(), 'ウィンドウにページが出ない').toBeVisible();

    // ウィンドウの根は position: fixed の要素。最大化の前は画面より小さく、後は画面いっぱいになる
    const windowSize = (btn: Element) => {
      let el: HTMLElement | null = btn as HTMLElement;
      while (el && getComputedStyle(el).position !== 'fixed') el = el.parentElement;
      // 開くときのアニメーションの縮小 (transform) を含まない大きさで測る
      return el ? { width: el.offsetWidth, height: el.offsetHeight, vw: window.innerWidth, vh: window.innerHeight } : null;
    };
    const before = await maximize.evaluate(windowSize);
    expect(before, 'ウィンドウの根が見つからない').not.toBeNull();
    expect(before!.width < before!.vw - 1 || before!.height < before!.vh - 1, `最大化の前から画面いっぱい (対照) ${JSON.stringify(before)}`).toBe(true);

    await maximize.click();
    await expect(restore, '最大化のボタンが元に戻すのボタンに変わらない').toHaveCount(1);
    await expect.poll(async () => {
      const after = await restore.evaluate(windowSize);
      return after != null && after.width >= after.vw - 1 && after.height >= after.vh - 1;
    }, { message: '最大化したウィンドウが画面いっぱいにならない' }).toBe(true);

    await restore.click();
    await expect(maximize, '元に戻せない').toHaveCount(1);
    await expect.poll(async () => {
      const restored = await maximize.evaluate(windowSize);
      return restored != null && (restored.width < restored.vw - 1 || restored.height < restored.vh - 1);
    }, { message: '元に戻しても画面いっぱいのまま' }).toBe(true);
    await maximize.locator('xpath=following-sibling::button[i[contains(@class, "ti-x")]]').click();
    await expect(maximize, 'ウィンドウが閉じない').toHaveCount(0);
  });
});
