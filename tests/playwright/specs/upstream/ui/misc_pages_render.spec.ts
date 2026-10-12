/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 単発の未カバーページをまとめて開く (#2441)。
//
//   /my/achievements   実績
//   /admin/file/:fileId ドライブファイルの管理画面
//   /:(*)              not-found
//
// いずれも 1 ページで完結し、専用の spec を立てるほどの操作を持たないので
// 1 ファイルにまとめる。個別に増えすぎると spec の見通しが悪くなる。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { uploadTinyPNG } from '../../../fixtures/files';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: 単発ページ', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('/my/achievements が実績一覧を出す', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/achievements`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await expect(page.getByText('Achievements', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('/admin/file/:fileId がファイルの詳細を出す', async ({ page, baseURL, request }) => {
    // **ドライブは同一内容のファイルを MD5 で重複排除する。** 他の spec が同じ
    // tinyPNG を上げていると、渡した名前ではなく既存ファイルがそのまま返る。
    // 照合は「渡した名前」ではなく **レスポンスが返した名前**で行うこと。
    const file = await uploadTinyPNG(
      request,
      baseURL!,
      root.token,
      `pw-adminfile-${Date.now().toString().slice(-9)}.png`,
    );

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/file/${file.id}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // この画面は drive/files/show と admin/drive/show-file を **両方**読む。
    // 後者はモデレーター専用で、権限判定が壊れていると片方だけ落ちて
    // 画面が途中までしか出ない。
    await expect(page.getByText(file.name, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('存在しない URL は not-found を出す', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/pw-no-such-page-${Date.now()}`, {
      waitUntil: 'domcontentloaded',
    });
    // SPA なので HTTP は 200 で、画面側で not-found を出す。
    expect(resp!.status()).toBe(200);

    // catchall が効いていないと白画面になる。利用者はリンク切れか障害かを
    // 区別できない。
    await expect(
      page.getByText('No page corresponding to this URL could be found.', { exact: false }).first(),
    ).toBeVisible({ timeout: 20_000 });
  });
});
