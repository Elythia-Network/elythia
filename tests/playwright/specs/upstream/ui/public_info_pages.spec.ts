/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// インスタンスの公開情報ページをまとめて開く (#2441)。
//
//   /ads                    広告一覧
//   /avatar-decorations     アバターデコレーション
//   /custom-emojis-manager  カスタム絵文字の管理
//   /roles/:roleId          公開ロールのページ
//
// いずれも管理画面 (`/admin/*`) とは別の、利用者側から見える入口。管理画面が
// 出ても公開側が壊れているという壊れ方があるので、両方を見る必要がある。
//
// `/roles/:roleId` は **isPublic かつ isExplorable のロールだけ**中身を出す。
// どちらかが false だと「何もない」表示になるため、spec 側で両方を立てる。
// ここを取り違えると、正常なのに空という誤検知になる。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { cleanupApi } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: 公開情報ページ', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  // 有効な広告はタイムラインなどにも出るので、テストが時間切れで終わっても afterEach で消す
  let adId: string | undefined;
  test.afterEach(async ({ request }) => {
    if (adId === undefined) return;
    await cleanupApi(request, 'admin/ad/delete', { i: root.token, id: adId });
    adId = undefined;
  });

  test('/ads shows an active ad', coversRoutes('/ads'), async ({ page, baseURL, request }) => {
    // 有効な広告を 1 つ作る。見出しの文言だけでは、一覧が空でも通ってしまう (#4072)
    const imageUrl = `https://remote.example/pw-ads-${Date.now().toString(36)}.png`;
    const created = await callApi(request, 'admin/ad/create', {
      i: root.token, url: 'https://remote.example/pw-ads', memo: 'pw /ads', place: 'square', priority: 'middle', ratio: 1,
      startsAt: Date.now() - 60_000, expiresAt: Date.now() + 3_600_000, imageUrl, dayOfWeek: 0,
    });
    expect(created.status()).toBe(200);
    adId = ((await created.json()) as { id: string }).id;
    await uiSigninAsRoot(page, baseURL, root);
    await gotoRoute(page, baseURL!, '/ads');
    await expect(page.locator(`img[src="${imageUrl}"]`).first(), '作った広告が出ない').toBeVisible({ timeout: 20_000 });
  });

  test('/avatar-decorations がデコレーション一覧を出す', coversRoutes('/avatar-decorations'), async ({ page, baseURL, request }) => {
    // 1 件登録しておく。空一覧だと「描画できている」のか「取得に失敗して空」
    // なのか区別できない。
    const name = `pwdeco${Date.now().toString().slice(-9)}`;
    const created = await callApi(request, 'admin/avatar-decorations/create', {
      i: root.token,
      name,
      description: 'playwright fixture',
      url: 'https://example.invalid/pw-deco.png',
    });
    expect(created.status()).toBeLessThan(400);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/avatar-decorations`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    await expect(page.getByText(name, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
  });

  test('/custom-emojis-manager が絵文字管理を出す', coversRoutes('/custom-emojis-manager'), async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await gotoRoute(page, baseURL!, '/custom-emojis-manager');

    await expect(page.getByText('Custom Emoji', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('/roles/:roleId が公開ロールを出す', coversRoutes('/roles/:roleId'), async ({ page, baseURL, request }) => {
    const name = `pw-public-role-${Date.now().toString().slice(-9)}`;
    const created = await callApi(request, 'admin/roles/create', {
      i: root.token,
      name,
      description: 'playwright fixture',
      color: null,
      iconUrl: null,
      target: 'manual',
      condFormula: {},
      // 公開ページに出すには両方が必要。
      isPublic: true,
      isExplorable: true,
      isAdministrator: false,
      isModerator: false,
      asBadge: false,
      canEditMembersByModerator: false,
      displayOrder: 0,
      policies: {},
    });
    expect(created.status()).toBe(200);
    const role = (await created.json()) as { id: string };

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/roles/${role.id}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await expect(page.getByText(name, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
  });
});
