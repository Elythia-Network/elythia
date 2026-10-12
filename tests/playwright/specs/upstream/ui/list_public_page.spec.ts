/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /list/:listId (公開リストのページ) をブラウザで操作する (#2441)。
//
// `/my/lists/:listId` (自分のリストの編集画面) とは **別のページ**で、こちらは
// 他人が見る公開ページ。メンバー一覧の表示と「いいね」しかできない。
//
// このページは `users/lists/show` を **forPublic: true** で叩く。公開していない
// リストは取得できず error 表示になるため、spec では明示的に isPublic を立てる。
// ここを取り違えると「404 ではないのに何も出ない」形で落ちる。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

interface CreatedList {
  id: string;
  name: string;
  memberUsername: string;
}

/** Create a public list owned by root with one member. */
async function createPublicList(
  request: import('@playwright/test').APIRequestContext,
  root: RootFixture,
): Promise<CreatedList> {
  const name = `pw-list-${Date.now().toString().slice(-9)}`;
  const created = await callApi(request, 'users/lists/create', { i: root.token, name });
  expect(created.status()).toBe(200);
  const list = (await created.json()) as { id: string };

  const member = await signupUser(request, randomUsername('listmem'), DEFAULT_TEST_PASSWORD);
  const pushed = await callApi(request, 'users/lists/push', {
    i: root.token,
    listId: list.id,
    userId: member.id,
  });
  expect(pushed.status()).toBe(204);

  // 公開しないと forPublic: true の取得が通らない。
  const updated = await callApi(request, 'users/lists/update', {
    i: root.token,
    listId: list.id,
    isPublic: true,
  });
  expect(updated.status()).toBe(200);

  return { id: list.id, name, memberUsername: member.username };
}

test.describe('UI: /list/:listId public list page', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('公開リストの名前とメンバーが表示される', async ({ page, baseURL, request }) => {
    const list = await createPublicList(request, root);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/list/${list.id}`, { waitUntil: 'domcontentloaded' });

    await expect(page.getByText(list.name, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
    // メンバーが出ないと「誰が入っているか分からないリスト」になる。
    await expect(page.getByText(list.memberUsername, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('いいねを押すと users/lists/favorite が呼ばれる', async ({ page, baseURL, request }) => {
    const list = await createPublicList(request, root);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/list/${list.id}`, { waitUntil: 'domcontentloaded' });
    await expect(page.getByText(list.name, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });

    // 未いいねの間だけ ti-heart が出る (いいね済みは ti-heart-off)。
    const likeButton = page.locator('button:has(i.ti-heart)').first();
    await expect(likeButton).toBeVisible({ timeout: 20_000 });

    // upstream の `users/lists/favorite` は `res: void` なので **204** が返る。
    // 200 で待つと成功しているのに timeout する。
    const favorited = page.waitForResponse(
      (r) => r.url().includes('/api/users/lists/favorite') && r.status() === 204,
      { timeout: 20_000 },
    );
    await likeButton.click();
    await favorited;

    // サーバー側にも反映されている。画面の見た目だけ変わって実体が付いて
    // いないと、再読込で消える。
    await expect(async () => {
      const shown = await callApi(request, 'users/lists/show', {
        i: root.token,
        listId: list.id,
        forPublic: true,
      });
      expect(shown.status()).toBe(200);
      expect(((await shown.json()) as { isLiked?: boolean }).isLiked).toBe(true);
    }).toPass({ timeout: 15_000 });
  });

  test('非公開のリストは公開ページで取得できない', async ({ page, baseURL, request }) => {
    const name = `pw-private-${Date.now().toString().slice(-9)}`;
    const created = await callApi(request, 'users/lists/create', { i: root.token, name });
    expect(created.status()).toBe(200);
    const list = (await created.json()) as { id: string };

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/list/${list.id}`, { waitUntil: 'domcontentloaded' });

    // 非公開リストが公開ページから見えると、メンバーが第三者に漏れる。
    // 所有者本人が開いても forPublic 経路では出さないのが upstream の挙動。
    await expect(page.getByText(name, { exact: true })).toHaveCount(0, { timeout: 20_000 });
  });
});
