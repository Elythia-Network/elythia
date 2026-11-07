/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/files の絞り込み (#4072)。2 人の利用者にファイルを上げ、User ID で絞ると片方のファイルだけが
// 残り、さらに MIME type で別の種類に絞ると消えることを確かめる。一覧はサムネイルだけで、各ファイルは
// /admin/file/<id> へのリンクなので、リンクで探す。
//
// 絞り込みを変えると、古い一覧がフェードで残り、次にローディング (リンク 0 件)、新しい一覧の順に
// 移る。どちらの途中でも成り立たない組 (片方だけが残る / 空の表示が出る) で見る。

import { expect, test } from '@playwright/test';
import { fillField, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { uploadTinyPNG } from '../../../fixtures/files';
import { pageBody } from '../../../fixtures/route_check';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { freshUser } from '../../../fixtures/ui_timeline';

test.describe('UI: /admin/files', () => {
  test.setTimeout(60_000);

  test('filter files by user and MIME type', coversRoutes('/admin/files'), async ({ page, baseURL, request }) => {
    const a = await freshUser(request, 'pwafa');
    const b = await freshUser(request, 'pwafb');
    const fileA = await uploadTinyPNG(request, baseURL!, a.token, 'pw-a.png');
    // いちばん新しいので、絞る前の 1 ページ目に必ず出る
    const fileB = await uploadTinyPNG(request, baseURL!, b.token, 'pw-b.png');

    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/files');
    const linkA = pageBody(page).locator(`a[href="/admin/file/${fileA.id}"]`);
    const linkB = pageBody(page).locator(`a[href="/admin/file/${fileB.id}"]`);
    await expect(linkB, '絞る前は新しいファイルが出る (対照)').toHaveCount(1, { timeout: 20_000 });

    const byUser = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/drive/files' && r.request().postDataJSON()?.userId === a.id, { timeout: 15_000 });
    await fillField(page, 'User ID', a.id);
    expect((await byUser).ok(), 'admin/drive/files が失敗した').toBe(true);
    await expect.poll(async () => [await linkA.count(), await linkB.count()], { message: '絞った利用者のファイルだけにならない' }).toEqual([1, 0]);

    const byType = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/drive/files' && r.request().postDataJSON()?.type === 'text/plain', { timeout: 15_000 });
    await fillField(page, 'MIME type', 'text/plain');
    expect((await byType).ok(), 'admin/drive/files が失敗した').toBe(true);
    // ローディング中もリンクは 0 件なので、空の表示 (MkResult) が出たことまで見る
    const empty = pageBody(page).getByText("There's nothing to see here");
    await expect.poll(async () => [await linkA.count(), await empty.count() > 0], { message: '別の種類のファイルが残る' }).toEqual([0, true]);
  });
});
