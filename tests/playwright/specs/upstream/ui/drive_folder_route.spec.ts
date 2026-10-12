/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/drive/folder/:folder をブラウザで開く (#2441)。
//
// **このルートは現状フォルダを開かない。** `drive.vue` は `MkDrive` に
// `initialFolder` を渡しておらず、`:folder` は誰にも読まれない。実際に開くと
// 200 でドライブの **ルート**が描画される (フォルダ名はパンくずではなく一覧側に
// 並ぶ)。fork は `drive.vue` を変更していないので upstream の挙動そのもの。
//
// したがってここで検証できるのは「ルートが解決してドライブ UI が hydrate する」
// ことまで。フォルダの中身を assert する spec を書くと、mk-go でも TS backend でも
// 落ちる (frontend の挙動なので backend を差し替えても変わらない)。
//
// upstream が deep link を実装したらこの spec は通り続けるが、そのときは
// 中身の assert を足せる。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /my/drive/folder/:folder', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('フォルダ ID 付きで開いてもドライブ UI が描画される', async ({
    page,
    baseURL,
    request,
  }) => {
    const name = `pwfolder${Date.now().toString().slice(-6)}`;
    const created = await callApi(request, 'drive/folders/create', { i: root.token, name });
    expect(created.status()).toBe(200);
    const folder = (await created.json()) as { id: string };

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/drive/folder/${folder.id}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // 作成したフォルダが一覧に出る = drive/files, drive/folders の取得まで
    // 到達している。ここが空だと「ページは出るが中身が無い」状態を見逃す。
    await expect(page.getByText(name, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
  });

  test('存在しないフォルダ ID でも画面が壊れない', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/drive/folder/aaaaaaaaaaaaaaaa`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // ドライブの UI 自体は出る (param が使われないので当然だが、将来 deep link が
    // 実装されたときに「不正な ID で白画面」になる退行を検出できる)。
    await expect(page.getByText('Drive', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});
