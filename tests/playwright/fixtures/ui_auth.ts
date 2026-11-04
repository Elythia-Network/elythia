/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// UI 操作 spec で root 認証を行う共通 helper。
//
// **root はフォームでサインインしない (#4026)。** globalSetup が 1 回だけ
// フォームでサインインし、その時点の storage state (localStorage と IndexedDB)
// を `.auth/root-state.json` に保存する。`uiSigninAsRoot` はそれを
// `context.setStorageState()` で読み込む (`account` だけは今の値に差し替える)。
// spec ごとにフォームを通すと 1 本あたり数秒かかり、呼ぶ spec が 179 本ある。
// frontend はサインイン中の利用者を localStorage の `account` から読む
// (frontend/.../src/i.ts) ので、フォームを通した直後と同じ状態から始まる。
//
// フォームの経路そのものは `uiSigninViaForm` に残し、globalSetup と
// `uiSigninAs` (spec ごとに登録した利用者) が使う。
//
// 使い方:
//   import { uiSigninAsRoot, RootFixture } from '../../fixtures/ui_auth';
//   await uiSigninAsRoot(page, baseURL, root);
//
// pattern:
// - viewport 1600x900 で navbar collapse を回避
// - data-testid selector で fragility が低い (upstream Misskey が e2e 用に
//   付けている属性をそのまま使う)
// - 完了条件: navbar の data-testid="open-post-form" が visible (=
//   authenticated home の hydration 完了 = signin が backend → frontend まで通った)
//
// 注: upstream は 2026.7.0 で `data-cy-*` 属性を全廃し `data-testid="..."` に
// 移行した (例: `data-cy-signin` → `data-testid="signin"`)。旧属性のままだと
// signin form のクリック対象が見つからず、この helper を使う全 UI spec が
// 60s timeout で落ちる。

import { readFileSync } from 'node:fs';
import { type APIRequestContext, type BrowserContext, expect, type Page } from '@playwright/test';
import { callApi } from './api';
import { DEFAULT_TEST_PASSWORD, type Principal } from './auth';

export interface RootFixture {
  id: string;
  token: string;
  username: string;
}

/** Path of the storage state globalSetup saves after signing in as root through the form. */
export const ROOT_STATE_PATH = '.auth/root-state.json';

// フォームでサインインする。globalSetup と `uiSigninAs` 用で、spec からは
// 呼ばない (root なら `uiSigninAsRoot`、別の利用者なら `uiSigninAs` を使う)。
export async function uiSigninViaForm(page: Page, baseURL: string, username: string, password = DEFAULT_TEST_PASSWORD): Promise<void> {
  await page.setViewportSize({ width: 1600, height: 900 });
  await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });
  await page.locator('[data-testid="signin"]').first().click();
  await expect(page.locator('[data-testid="signin-page-input"]')).toBeVisible({ timeout: 10_000 });

  await page.locator('[data-testid="signin-username"] input').fill(username);
  await page.locator('[data-testid="signin-username"] input').press('Enter');
  await expect(page.locator('[data-testid="signin-page-password"]')).toBeVisible({ timeout: 10_000 });

  const signinResp = page.waitForResponse(
    (resp) => resp.url().includes('/api/signin-flow') && resp.status() === 200,
    { timeout: 15_000 },
  );
  await page.locator('[data-testid="signin-password"] input').fill(password);
  await page.locator('[data-testid="signin-password"] input').press('Enter');
  await signinResp;

  // hydration 完了の最終確認
  await expect(page.locator('[data-testid="open-post-form"]').first()).toBeVisible({ timeout: 15_000 });
}

// サインイン中の利用者が `id` であることを確かめる。注入した state が別の DB の
// root のもの (前の run の残り) だと、ホームは出ても別人として動くので、
// ここで落とす。
async function expectSignedInAs(page: Page, id: string): Promise<void> {
  const current = await page.evaluate(() => {
    const raw = window.localStorage.getItem('account');
    return raw === null ? null : (JSON.parse(raw) as { id?: string }).id ?? null;
  });
  expect(current, 'サインイン中の利用者が想定と違う').toBe(id);
}

// 保存した state の `account` を、今の root の値に差し替えたものを返す。
//
// frontend は起動時に localStorage の `account` をそのまま `$i` にし、サーバーから
// 取り直すのは後から待たずに行う (boot/common.ts の refreshCurrentAccount)。設定の
// 画面は mount の時点で `$i` の値を写し取るので、保存した時点の古い値 (前の spec が
// 変えた isLocked など) で mount すると、表示も保存する値も古くなる。フォームで
// サインインしていたときは毎回新しい値だったので、それと同じにする。
async function freshRootState(page: Page, baseURL: string, root: RootFixture): Promise<Exclude<Parameters<BrowserContext['setStorageState']>[0], string>> {
  const saved = JSON.parse(readFileSync(ROOT_STATE_PATH, 'utf-8')) as Exclude<Parameters<BrowserContext['setStorageState']>[0], string>;
  const resp = await page.request.post(`${baseURL}/api/i`, { data: { i: root.token }, failOnStatusCode: false });
  expect(resp.ok(), `i failed: ${resp.status()}`).toBe(true);
  const account = JSON.stringify({ ...(await resp.json()), token: root.token });
  let replaced = 0;
  for (const origin of saved.origins ?? []) {
    for (const item of origin.localStorage ?? []) {
      if (item.name === 'account') {
        item.value = account;
        replaced++;
      }
    }
  }
  // 保存の形が変わって差し替えが空振りすると、古い値のまま黙って進むので落とす
  expect(replaced, `${ROOT_STATE_PATH} has no localStorage account`).toBe(1);
  return saved;
}

/** Starts the page signed in as root by loading the state globalSetup saved, with root's current account data. */
export async function uiSigninAsRoot(page: Page, baseURL: string, root: RootFixture): Promise<void> {
  await page.setViewportSize({ width: 1600, height: 900 });
  await page.context().setStorageState(await freshRootState(page, baseURL, root));
  await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });
  await expect(page.locator('[data-testid="open-post-form"]').first()).toBeVisible({ timeout: 15_000 });
  await expectSignedInAs(page, root.id);
}

/**
 * Signs in through the form as a user the spec registered itself, so the spec
 * does not share root's account state with other specs.
 */
export async function uiSigninAs(page: Page, request: APIRequestContext, baseURL: string, user: Principal, password = DEFAULT_TEST_PASSWORD): Promise<void> {
  // 初回のウィザードが出ると、その上の modal が後続の click を奪う。root と
  // 同じく globalSetup と同じ値で抑止する (global-setup.ts の 4)
  const wizard = await callApi(request, 'i/registry/set', {
    i: user.token,
    scope: ['client', 'base'],
    key: 'accountSetupWizard',
    value: -1,
  });
  expect(wizard.ok(), `i/registry/set failed: ${wizard.status()}`).toBe(true);
  await uiSigninViaForm(page, baseURL, user.username, password);
  await expectSignedInAs(page, user.id);
}
