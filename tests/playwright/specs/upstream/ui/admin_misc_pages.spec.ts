/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の未カバーページと、単発のユーティリティページを開く (#2441)。
//
//   /admin                       管理画面の index
//   /admin/emojis2               絵文字管理 (新 UI)
//   /admin/federation-job-queue  連合ジョブキュー
//   /install-extensions          テーマ / プラグインの外部インストール
//   /user-tags/:tag              タグに紐づくユーザー一覧
//
// 判定にはページ固有の文言を使う。管理画面も左メニューを共有しているので、
// メニューの文言で assert するとどの `/admin/*` でも通る偽陽性になる。
// 文言は実際に全ページを開いて index の表示との差分から採った。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: 管理・ユーティリティの未カバーページ', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('/admin が管理メニューを出す', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // index の中身はメニューそのもの。ルートが壊れれば not-found になるので、
    // メニューが出ること自体が意味を持つ。
    // 実際の文言は Dashboard / Users / Roles / Job Queue など (Overview では
    // ない)。決め打ちせず、開いて確認したものを使う。
    for (const item of ['Dashboard', 'Users', 'Roles', 'Job Queue', 'Moderation logs']) {
      await expect(page.getByText(item, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
    }
  });

  test('/admin/emojis2 が絵文字管理を出す', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/emojis2`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await expect(page.getByText('Emoji registration', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('/admin/federation-job-queue が連合キューを出す', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/federation-job-queue`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // deliver / inbox の滞留はここでしか見えない。出ないと配送が詰まっていても
    // 気付けない。
    await expect(page.getByText('Errored instances', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('/install-extensions は情報不足を明示する', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/install-extensions`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // 外部サイトから渡されるパラメータが無いと成立しない画面。黙って白画面に
    // なるのではなく、理由を出すのが正しい挙動。
    await expect(
      page
        .getByText('There is not enough information to load data from an external site.', {
          exact: false,
        })
        .first(),
    ).toBeVisible({ timeout: 20_000 });
  });

  test('/user-tags/:tag がタグのユーザー一覧を出す', async ({ page, baseURL }) => {
    const tag = `pwtag${Date.now().toString().slice(-9)}`;

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/user-tags/${tag}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // タグ名が出る = URL のパラメータを拾えている。該当ユーザーは居ないので
    // 一覧は空になる。
    await expect(page.getByText(tag, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText('There are no users', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});
