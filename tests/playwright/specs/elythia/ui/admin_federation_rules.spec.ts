/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 連合のルール (#3090) と、連合の画面の Elythia のタブを通す (#4072)。ルールは /admin/federation の
// ルールのタブで足し、名前を変えて保存し、消す。足したルールは「記録だけ」で始まり、対象は
// .invalid のサーバーなので、並んで流れる他の spec の連合には効かない。
//
// 配送の健全性と消えたサーバーのタブは、このスタックには連合の相手がいないので、開いて API が
// 応え、読めたときの表示 (集計の欄 / 「消えたサーバーは無い」) が出ることまでを見る。

import { expect, type Page, test } from '@playwright/test';
import { cleanupApi, expectApiOk, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { isTsBackend } from '../../../fixtures/backend';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

type Rule = { id: string; name: string; hosts: string[] };

async function rules(page: Page): Promise<Rule[]> {
  return await expectApiOk(page.request, 'admin/federation/rules/list', { i: rootFixture().token }) as Rule[];
}

/** Opens a folder (MkFolder) when it is closed. */
async function openFolder(page: Page, testId: string, id: string) {
  const folder = page.locator(`[data-testid="${testId}"][data-rule-id="${id}"]`);
  if ((await folder.getAttribute('aria-expanded')) !== 'true') await folder.getByTestId('folder-header').first().click();
  await expect(folder).toHaveAttribute('aria-expanded', 'true');
  return folder;
}

test.describe('admin: federation', () => {
  test.skip(isTsBackend, '連合のルールは Elythia だけにある');
  test.setTimeout(90_000);

  test('add, rename and delete a federation rule', coversRoutes('/admin/federation'), async ({ page, baseURL }) => {
    const root = rootFixture();
    const name = `pw rule ${Date.now().toString(36)}`;
    let id: string | undefined;
    try {
      await signinAdmin(page, baseURL!, root);
      await gotoRoute(page, baseURL!, '/admin/federation');
      await page.getByTestId('federation-tab-rules').click();

      await page.getByTestId('federation-rule-add').click();
      const draft = page.getByTestId('federation-rule-draft');
      await draft.getByTestId('federation-rule-name').locator('input').fill(name);
      await draft.getByTestId('federation-rule-hosts').locator('textarea').fill('pw-rule.invalid');
      const created = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/federation/rules/create', { timeout: 15_000 });
      await draft.getByTestId('federation-rule-save').click();
      const createdResp = await created;
      expect(createdResp.ok(), 'admin/federation/rules/create が失敗した').toBe(true);
      id = (await createdResp.json() as Rule).id;
      const rule = (await rules(page)).find((r) => r.id === id);
      expect(rule?.name, '足したルールが一覧に無い').toBe(name);
      expect(rule!.hosts).toEqual(['pw-rule.invalid']);

      // 名前を変えて保存する
      const folder = await openFolder(page, 'federation-rule', id);
      await folder.getByTestId('federation-rule-name').locator('input').fill(`${name} renamed`);
      const updated = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/federation/rules/update', { timeout: 15_000 });
      await folder.getByTestId('federation-rule-save').click();
      expect((await updated).ok(), 'admin/federation/rules/update が失敗した').toBe(true);
      await expect.poll(async () => (await rules(page)).find((r) => r.id === id)?.name, { message: '名前の変更が保存されない' }).toBe(`${name} renamed`);

      // 消す
      const reopened = await openFolder(page, 'federation-rule', id);
      await reopened.getByTestId('federation-rule-delete').click();
      const deleted = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/federation/rules/delete', { timeout: 15_000 });
      await page.getByTestId('modal-dialog-ok').click();
      expect((await deleted).ok(), 'admin/federation/rules/delete が失敗した').toBe(true);
      await expect.poll(async () => (await rules(page)).some((r) => r.id === id), { message: '消したルールが残る' }).toBe(false);
      await expect(page.locator(`[data-testid="federation-rule"][data-rule-id="${id}"]`)).toHaveCount(0);
      id = undefined;
    } finally {
      if (id !== undefined) await cleanupApi(page.request, 'admin/federation/rules/delete', { i: root.token, ruleId: id });
    }
  });

  // 健全性の集計の欄は読み込み中から出ているので、読み終わった (data-fetching が false) ことまで見る。
  // 読めなかったときは欄ごと消えて「読めない」の表示になる
  for (const [tab, endpoint, shown, attr] of [
    ['federation-tab-deliver', 'admin/federation/delivery-health', 'federation-health-summary', ['data-fetching', 'false']],
    ['federation-tab-gone', 'admin/federation/gone-instances', 'gone-instances-none', null],
  ] as const) {
    test(`the ${tab.replace('federation-tab-', '')} tab loads`, coversRoutes('/admin/federation'), async ({ page, baseURL }) => {
      await signinAdmin(page, baseURL!, rootFixture());
      await gotoRoute(page, baseURL!, '/admin/federation');
      const loaded = page.waitForResponse((r) => new URL(r.url()).pathname === `/api/${endpoint}`, { timeout: 15_000 });
      await page.getByTestId(tab).click();
      expect((await loaded).ok(), `${endpoint} が失敗した`).toBe(true);
      // 「読めない」の表示ではなく、読めたときの表示が出る
      await expect(page.getByTestId(shown)).toBeVisible();
      if (attr !== null) await expect(page.getByTestId(shown)).toHaveAttribute(attr[0], attr[1]);
    });
  }
});
