/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 全ルートの表で開くことしか確かめていなかった画面の操作 (#4072)。
//
//   /clicker                 クッキーを押すと数が増え、i/registry に保存される
//   /qr                      自分の QR (canvas) と acct が出る (読み取りはカメラが要るので外す)
//   /preview                 見本の MFM の描画 (自分へのメンション、ハッシュタグ) とダイアログ
//   /settings/theme/manage   入れたテーマを選んで削除すると、端末の設定から消える
//   /settings/statusbar      ステータスバーを足して種類を選ぶと、端末の設定に入りページの外のバーに出る
//                            (このスタックは連合しないので「Federation」は選べない。RSS は外部の取得が
//                            要るので、手元で完結する「Lists」を使う。設定の画面の枠の見出しにも名前が
//                            出るので、バーは pageBody の外で探す)
//
// テーマとステータスバーは端末の設定 (localStorage の preferences) にだけ入り、API を呼ばない
// ので、readPref で確かめる。どれもこのテストで作った利用者で開く。

import { expect, type Page, test } from '@playwright/test';
import { fieldByLabel, pickSelect } from '../../../fixtures/admin_table';
import { pageBody } from '../../../fixtures/route_check';
import { readPref, signinWithState } from '../../../fixtures/settings_table';
import { clickButtonContainingText } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser } from '../../../fixtures/ui_timeline';

/** Counts the elements outside the routed page (pageBody) whose own text is exactly `text`. */
async function countTextOutsidePage(page: Page, text: string): Promise<number> {
  return page.evaluate((t) => {
    const body = Array.from(document.querySelectorAll('._pageContainer')).at(-1);
    return Array.from(document.querySelectorAll('body *')).filter((el) => !body?.contains(el)
      && Array.from(el.childNodes).some((n) => n.nodeType === Node.TEXT_NODE && n.textContent?.trim() === t)).length;
  }, text);
}

test.describe('UI: route operations (misc)', () => {
  test.setTimeout(60_000);

  test('clicker counts clicks and saves them', coversRoutes('/clicker'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwclk');
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/clicker');
    const count = page.getByTestId('count');
    await expect(count).toHaveText(/\b0\b/, { timeout: 15_000 });
    // クッキーのボタンは文字も testid も無い。画像の src (cookie.png) で探すと当たらなかったので、
    // 件数の表示の直後のボタンで取る
    const cookie = count.locator('xpath=following-sibling::button[1]');
    // 押すたびにアニメーションが掛かり、Playwright の「止まっている」の判定を待ち続けるので、要素の click() で押す
    for (let n = 0; n < 3; n++) await cookie.evaluate((b) => (b as HTMLButtonElement).click());
    await expect(count, '押した数だけ増えない').toHaveText(/\b3\b/);

    // 値が変わっていれば 5 秒ごとに保存する。page.goto で離れると読み込み直しになり、アンマウントでの
    // 保存は走らないので、定期の保存を待つ
    await expect.poll(async () => ((await apiOk(request, 'i/registry/get', { i: me.token, scope: ['clickerGame'], key: 'saveData' })) as { cookies: number }).cookies, { message: '押した数が保存されない', timeout: 15_000 }).toBe(3);
  });

  test('qr shows my code and acct', coversRoutes('/qr'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwqr');
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/qr');
    // 本家 2026.11.0 から QR は canvas ではなく、etiket/qr が作る SVG をそのまま描く (#4164)
    await expect(pageBody(page).locator('svg').first(), 'QR が描かれない').toBeVisible({ timeout: 15_000 });
    await expect(pageBody(page).getByText(`@${me.username}@${new URL(baseURL!).host}`).first(), '自分の acct が出ない').toBeVisible();
  });

  test('preview renders MFM and opens a dialog', coversRoutes('/preview'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwpv');
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/preview');
    // 見本の文にはサインイン中の利用者へのメンションと #misskey が入っている
    await expect(pageBody(page).locator(`a[href="/@${me.username}"]`).first(), '自分へのメンションが描かれない').toBeVisible({ timeout: 15_000 });
    await expect(pageBody(page).locator('a[href="/tags/misskey"]').first(), 'ハッシュタグが描かれない').toBeVisible();
    await clickButtonContainingText(page, 'Open dialog');
    await page.getByTestId('modal-dialog-ok').click();
    await expect(page.getByTestId('modal-dialog-ok')).toHaveCount(0);
  });

  test('uninstall an installed theme', coversRoutes('/settings/theme/manage'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwthm');
    const id = `pw-theme-${Date.now().toString(36)}`;
    const name = `PW theme ${Date.now().toString(36)}`;
    await signinWithState(page, request, baseURL!, me, { themes: [{ id, name, author: 'playwright', base: 'light', props: { accent: '#ff0000' } }] });
    await gotoRoute(page, baseURL!, '/settings/theme/manage');
    const ids = async () => ((await readPref(page, 'themes')) as { id: string }[] | undefined ?? []).map((t) => t.id);
    expect(await ids(), '入れたテーマが無い (前提)').toContain(id);
    await pickSelect(page, page.getByTestId('settings-theme-manage'), name);
    await clickButtonContainingText(page, 'Uninstall');
    await expect.poll(ids, { message: '削除したテーマが残る' }).not.toContain(id);
  });

  test('add a statusbar and choose its type', coversRoutes('/settings/statusbar'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwsb');
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/settings/statusbar');
    const statusbars = async () => ((await readPref(page, 'statusbars')) as { type: string | null }[] | undefined) ?? [];
    expect(await statusbars(), '初めはステータスバーが無い (前提)').toEqual([]);

    await page.getByTestId('settings-statusbar').getByRole('button', { name: 'Add' }).click();
    await expect.poll(async () => (await statusbars()).length, { message: 'ステータスバーが足されない' }).toBe(1);
    await page.getByTestId('settings-statusbar').getByTestId('folder-header').first().click();
    await pickSelect(page, await fieldByLabel(page, 'Type'), 'Lists');
    await expect.poll(async () => (await statusbars())[0]?.type, { message: '種類が保存されない' }).toBe('userList');
    // 種類を選ぶと既定の名前が入り、ページの外 (内容の欄の上) のステータスバーに出る
    await expect.poll(() => countTextOutsidePage(page, 'LIST TL'), { message: 'ページの外のステータスバーに出ない' }).toBeGreaterThan(0);
  });
});
