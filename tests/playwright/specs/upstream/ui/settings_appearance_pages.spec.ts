/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 外観・拡張まわりの設定画面をまとめて開く (#2441)。
//
// いずれも表示が中心で操作は少ないので 1 ファイルにまとめ、table-driven で回す。
//
// **判定にはページ固有の文言を使う。** `/settings/*` は左メニューを共有して
// いるため、メニューの文言で assert するとどの URL でも通る偽陽性になる
// (`/settings/accounts` で実際にその状態を作ってしまった)。ここで使う文言は
// 全ページを実際に開いて、`/settings` (index) の表示との差分から採った。
//
// 除外したページ:
//
//   - `/settings/theme/manage`   インストール済みテーマが無いと index との差分が
//                                空になる。固有の文言が無く、上記の偽陽性になる
//   - `/settings/profiles`       同上 (保存済みプロファイルが無いと何も出ない)

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

interface PageCase {
  path: string;
  /** Text that appears only on this page (verified against the /settings index). */
  marker: string;
}

const PAGES: PageCase[] = [
  { path: '/settings/theme', marker: 'Built-in themes' },
  { path: '/settings/theme/install', marker: 'Theme code' },
  {
    path: '/settings/custom-css',
    marker: 'Entering improper values may cause the client to stop functioning normally.',
  },
  { path: '/settings/sounds', marker: 'Master volume' },
  { path: '/settings/navbar', marker: 'Navigation bar' },
  { path: '/settings/deck', marker: 'Always show main column' },
  {
    path: '/settings/emoji-palette',
    marker: 'You can register presets as palettes to display prominently in the emoji picker',
  },
  { path: '/settings/other', marker: 'Account Migration' },
  { path: '/settings/plugin', marker: 'Install plugins' },
  { path: '/settings/plugin/install', marker: 'Please do not install untrustworthy plugins.' },
  { path: '/settings/drive/cleaner', marker: 'Sorting order' },
];

test.describe('UI: 外観・拡張の設定画面', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  for (const { path, marker } of PAGES) {
    test(`${path} が固有の内容を描画する`, async ({ page, baseURL }) => {
      await uiSigninAsRoot(page, baseURL, root);
      const resp = await page.goto(`${baseURL}${path}`, { waitUntil: 'domcontentloaded' });
      expect(resp!.status()).toBe(200);

      await expect(page.getByText(marker, { exact: false }).first()).toBeVisible({
        timeout: 20_000,
      });
    });
  }

  test('/settings/statusbar が追加ボタンを出す', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/settings/statusbar`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // ステータスバー未設定だと一覧は空で、この画面固有の要素は追加ボタンだけ。
    await expect(page.getByRole('button', { name: 'Add' }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('/settings が設定メニューを出す', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/settings`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // index の中身はメニューそのもの。ルートが壊れれば not-found になるので、
    // メニューが出ること自体が意味を持つ。
    for (const item of ['Profile', 'Privacy', 'Security', 'Preferences', 'Drive']) {
      await expect(page.getByText(item, { exact: true }).first()).toBeVisible({ timeout: 20_000 });
    }
  });
});
