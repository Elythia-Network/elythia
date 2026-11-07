/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/emojis (カスタム絵文字の管理) で、API で登録した絵文字を探して編集する。
//
// 検索欄で名前を入れて絞り、出た 1 つを押して編集のウィンドウでカテゴリを変えて保存する (#4072)。
// 一覧は新しい順なので、対象より後に対照の絵文字を登録し、検索の後に対照が消えて対象だけが残ることを
// 見る (検索の前から対象は 1 ページ目に出ているので、対象が出ることだけでは検索を確かめられない)。ウィンドウは pageBody の
// 外に開き、MkInput はどれも datalist の属性を持つので、カテゴリの欄は名前の欄の次の入力欄で取る。同じ画面は
// /custom-emojis-manager にも割り当てられている。

import { expect, test } from '@playwright/test';
import { cleanupApi, expectApiOk, fillField, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { uploadTinyPNG } from '../../../fixtures/files';
import { pageBody } from '../../../fixtures/route_check';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

type Emoji = { id: string; name: string; category: string | null };

for (const route of ['/admin/emojis', '/custom-emojis-manager'] as const) {
  test.describe(`UI: ${route}`, () => {
    test.setTimeout(60_000);

    test('search an emoji by name and change its category', coversRoutes(route), async ({ page, baseURL, request }) => {
      const root = rootFixture();
      const file = await uploadTinyPNG(request, baseURL!, root.token, 'pw-emoji.png');
      const name = `pwemo${Date.now().toString(36)}`;
      const control = `pwctl${Date.now().toString(36)}`;
      const add = async (n: string) => (await expectApiOk(request, 'admin/emoji/add', {
        i: root.token, name: n, fileId: file.id, category: null, aliases: [], license: null,
        isSensitive: false, localOnly: false, roleIdsThatCanBeUsedThisEmojiAsReaction: [],
      }) as { id: string }).id;
      const ids: string[] = [];
      const find = async () => (await expectApiOk(request, 'admin/emoji/list', { i: root.token, query: name, limit: 10 }) as Emoji[]).find((e) => e.name === name);
      try {
        ids.push(await add(name));
        ids.push(await add(control));
        await signinAdmin(page, baseURL!, root);
        await gotoRoute(page, baseURL!, route);
        const button = pageBody(page).locator('button.emoji').filter({ has: page.locator(`img[alt="${name}"]`) });
        const controlButton = pageBody(page).locator('button.emoji').filter({ has: page.locator(`img[alt="${control}"]`) });
        await expect(controlButton, '検索の前は対照の絵文字が出る (対照)').toHaveCount(1, { timeout: 15_000 });
        const searched = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/emoji/list' && r.request().postDataJSON()?.query === name, { timeout: 15_000 });
        await fillField(page, 'Search', name);
        expect((await searched).ok(), 'admin/emoji/list が失敗した').toBe(true);
        await expect.poll(async () => [await button.count(), await controlButton.count()], { message: '検索で対象の絵文字だけにならない' }).toEqual([1, 0]);

        await button.click();
        const category = `pwcat${Date.now().toString(36)}`;
        // 名前の欄 (pattern で一意) の次の入力欄がカテゴリ
        const nameInput = page.locator('input[pattern="[a-z0-9_]"]');
        await expect(nameInput, '編集のウィンドウが開かない').toHaveValue(name);
        await nameInput.locator('xpath=following::input[1]').fill(category);
        const updated = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/emoji/update', { timeout: 15_000 });
        await page.locator('button:has(i.ti-check)').filter({ hasText: 'Update' }).click();
        expect((await updated).ok(), 'admin/emoji/update が失敗した').toBe(true);
        expect((await find())?.category, 'カテゴリの変更が保存されない').toBe(category);
      } finally {
        for (const id of ids) await cleanupApi(request, 'admin/emoji/delete', { i: root.token, id });
      }
    });
  });
}
