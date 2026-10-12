/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /gallery/new と /gallery/:postId/edit をブラウザで操作する (#2441)。
//
// 既存の `gallery_index_render.spec.ts` は一覧まで。**投稿と編集の画面**は
// 未検証だった。ギャラリーは画像そのものが中身なので、`gallery/posts/create`
// が 200 を返していても添付が画面に出ていなければ機能していない。
//
// 2 つのパスは同じ `edit.vue` を使い、`postId` の有無で新規 / 編集を切り替える。
// ボタンの文言も Publish / Save で変わるため、両方を踏む。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { uploadTinyPNG } from '../../../fixtures/files';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

/** Fill a Vue-bound input/textarea the way v-model actually observes. */
async function setFieldValue(
  page: import('@playwright/test').Page,
  locator: import('@playwright/test').Locator,
  value: string,
): Promise<void> {
  await locator.click();
  await locator.fill(value);
  // fill() は input イベントを出すが、念のため v-model 更新を待つ。
  await expect(locator).toHaveValue(value, { timeout: 10_000 });
}

test.describe('UI: gallery post create / edit', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(90_000);

  test('/gallery/new は投稿フォームを出す', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/gallery/new`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // 新規なので Publish (編集時は Save)。ここが Save だと postId を拾って
    // しまっている。
    await expect(page.getByRole('button', { name: 'Publish' })).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText('Title', { exact: true }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('/gallery/:postId/edit は既存の内容を読み込み、保存できる', async ({
    page,
    baseURL,
    request,
  }) => {
    const file = await uploadTinyPNG(request, baseURL!, root.token, `pw-gallery-${Date.now()}.png`);
    const title = `pw-gallery-${Date.now().toString().slice(-9)}`;
    const created = await callApi(request, 'gallery/posts/create', {
      i: root.token,
      title,
      description: 'playwright fixture',
      fileIds: [file.id],
      isSensitive: false,
    });
    expect(created.status()).toBe(200);
    const post = (await created.json()) as { id: string };

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/gallery/${post.id}/edit`, { waitUntil: 'domcontentloaded' });

    // 既存投稿なので Save 側。
    await expect(page.getByRole('button', { name: 'Save' })).toBeVisible({ timeout: 20_000 });

    // 既存のタイトルが入力欄に入っている = gallery/posts/show を読めている。
    // ここが空だと、保存した瞬間にタイトルが消える。
    // MkInput は type 属性を省いて render するので `input[type="text"]` では
    // 当たらない (既存 spec でも踏んでいる罠)。role で取る。
    const titleInput = page.getByRole('textbox').first();
    await expect(titleInput).toHaveValue(title, { timeout: 20_000 });

    // 添付画像が出る。ギャラリーは画像が中身なので、ここが空なら壊れている。
    await expect(page.locator('img').filter({ visible: true }).first()).toBeVisible({
      timeout: 20_000,
    });

    const renamed = `${title}-edited`;
    await setFieldValue(page, titleInput, renamed);

    const updated = page.waitForResponse(
      (r) => r.url().includes('/api/gallery/posts/update') && r.status() < 400,
      { timeout: 20_000 },
    );
    await page.getByRole('button', { name: 'Save' }).click();
    await updated;

    // サーバー側にも反映されている。
    await expect(async () => {
      const shown = await callApi(request, 'gallery/posts/show', { postId: post.id });
      expect(shown.status()).toBe(200);
      expect(((await shown.json()) as { title: string }).title).toBe(renamed);
    }).toPass({ timeout: 15_000 });
  });
});
