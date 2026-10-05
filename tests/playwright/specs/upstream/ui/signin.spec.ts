/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// UI 操作で signin → 認証済 home の hydration を verify する spec。
//
// upstream Misskey の Cypress と同じ data-cy-* selector を使う (=
// 本家の `cypress/support/commands.ts` を参照)。Vue 内部 class
// 名 / id は build ごとに変わるが data-cy-* は明示的に test 用 fixture と
// して維持されているので fragility が低い。
//
// 本 spec は以下の段階を実 browser で踏む:
//   1. globalSetup で作成済 root (alice / DEFAULT_TEST_PASSWORD) の credential
//      を使って signin form を開く
//   2. username + password を form に入力 → submit
//   3. /api/signin-flow が 200 で完了
//   4. 認証済 home に navbar (= [data-testid="open-post-form"] を含む) が hydrate
//      されることを verify
//
// composer modal 経由で note 投稿する write-flow は post_note.spec.ts 側で
// cover している。本 spec は signin form chain だけに focus する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { DEFAULT_TEST_PASSWORD } from '../../../fixtures/auth';
import type { RootFixture } from '../../../fixtures/ui_auth';

test.describe('UI: signin via form', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  // SPA 内 click は CSS animation の停止待ち + overlay 排除で時間がかかる
  // ことがある。globalSetup 後の signin → form open → submit chain を 1 つの
  // test で測ると 30s default では足りないので 60s に延長。
  test.setTimeout(60_000);

  test('UI signin form completes and authenticated home renders', async ({ page, baseURL }) => {
    // 1. ホームを開く (logged-out state)
    await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });

    // 2. signin ボタンを click → signin modal が開く
    //    upstream Misskey は data-cy-signin button を home の login wall に置く
    await page.locator('[data-testid="signin"]').first().click();
    await expect(page.locator('[data-testid="signin-page-input"]')).toBeVisible({ timeout: 10_000 });

    // 3. username 入力 + Enter で password ステップに進む
    await page.locator('[data-testid="signin-username"] input').fill(root.username);
    await page.locator('[data-testid="signin-username"] input').press('Enter');
    await expect(page.locator('[data-testid="signin-page-password"]')).toBeVisible({ timeout: 10_000 });

    // 4. password 入力 + Enter で submit
    //    /api/signin-flow が叩かれて成功すれば SPA は home に遷移する
    const signinResp = page.waitForResponse(
      (resp) => resp.url().includes('/api/signin-flow') && resp.status() === 200,
      { timeout: 15_000 },
    );
    await page.locator('[data-testid="signin-password"] input').fill(DEFAULT_TEST_PASSWORD);
    await page.locator('[data-testid="signin-password"] input').press('Enter');
    await signinResp;

    // 5. 認証済 home が hydrate されたことを verify。
    //    Misskey は authenticated state で navbar に投稿ボタン
    //    (= data-cy-open-post-form) を render する。これが見えれば signin が
    //    backend → frontend hydration まで通った確認になる。
    await expect(page.locator('[data-testid="open-post-form"]').first()).toBeVisible({ timeout: 15_000 });
  });
});
