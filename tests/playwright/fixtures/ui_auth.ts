/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// UI 操作 spec で root 認証を行う共通 helper。
//
// signin form (data-testid="signin" / "signin-username" / "signin-password")
// を実 browser で driving して /api/signin-flow まで通す flow を集約する。
// 後続 spec はこの helper を import して認証済 state から始められる。
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

import { expect, type Page } from '@playwright/test';
import { DEFAULT_TEST_PASSWORD } from './auth';

export interface RootFixture {
  id: string;
  token: string;
  username: string;
}

// signin form 経由で alice として認証する。globalSetup が DEFAULT_TEST_PASSWORD
// で root を作成している前提 (tests/playwright/global-setup.ts)。
export async function uiSigninAsRoot(page: Page, baseURL: string, root: RootFixture): Promise<void> {
  await page.setViewportSize({ width: 1600, height: 900 });
  await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });
  await page.locator('[data-testid="signin"]').first().click();
  await expect(page.locator('[data-testid="signin-page-input"]')).toBeVisible({ timeout: 10_000 });

  await page.locator('[data-testid="signin-username"] input').fill(root.username);
  await page.locator('[data-testid="signin-username"] input').press('Enter');
  await expect(page.locator('[data-testid="signin-page-password"]')).toBeVisible({ timeout: 10_000 });

  const signinResp = page.waitForResponse(
    (resp) => resp.url().includes('/api/signin-flow') && resp.status() === 200,
    { timeout: 15_000 },
  );
  await page.locator('[data-testid="signin-password"] input').fill(DEFAULT_TEST_PASSWORD);
  await page.locator('[data-testid="signin-password"] input').press('Enter');
  await signinResp;

  // hydration 完了の最終確認
  await expect(page.locator('[data-testid="open-post-form"]').first()).toBeVisible({ timeout: 15_000 });
}
