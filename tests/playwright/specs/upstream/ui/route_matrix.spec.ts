/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// authenticated SPA route の網羅 navigation smoke。
//
// 既存 API spec で cover している endpoint の多くは frontend で対応 page を
// 持つ。本 spec は table-driven で **主要 page を全部 navigate して hydration
// が壊れていないこと** を smoke level で確認する。各 page の固有 element 検証
// は別 spec (= signin / post_note / authenticated_routes 等) に集約し、本 spec
// は wide-coverage の route mounts smoke として位置付ける。
//
// 完了判定: navbar の data-cy-open-post-form (= authenticated home) が visible。
// SPA hydration 完了 + auth state 維持 を 1 condition で確認できる。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

interface RouteCase {
  path: string;
  label: string;
}

const AUTH_ROUTES: RouteCase[] = [
  // timeline 系
  { path: '/timeline/home', label: 'timeline/home' },
  { path: '/timeline/global', label: 'timeline/global' },
  { path: '/timeline/hybrid', label: 'timeline/hybrid' },
  // discovery / explore
  // /explore は単体 route。tab は initialTab prop に hash 経由で渡されるが、
  // 連続 page.goto で hash だけ変えると Playwright は null response を返す
  // (= same-document navigation 扱い) ので、route_matrix では default tab
  // 経路だけを cover する。tab 別 hydration verify は専用 spec
  // (specs/ui/explore_users_render.spec.ts) で行う。
  { path: '/explore', label: 'explore (default = featured)' },
  // i / 自アカウント系
  { path: '/my/favorites', label: 'my/favorites' },
  { path: '/my/clips', label: 'my/clips' },
  { path: '/my/lists', label: 'my/lists' },
  { path: '/my/antennas', label: 'my/antennas' },
  { path: '/my/drive', label: 'my/drive' },
  // settings
  { path: '/settings', label: 'settings (root)' },
  { path: '/settings/general', label: 'settings/general' },
  { path: '/settings/privacy', label: 'settings/privacy' },
  { path: '/settings/security', label: 'settings/security' },
  { path: '/settings/2fa', label: 'settings/2fa' },
  { path: '/settings/account', label: 'settings/account' },
  { path: '/settings/api', label: 'settings/api' },
  { path: '/settings/import-export', label: 'settings/import-export' },
  { path: '/settings/word-mute', label: 'settings/word-mute' },
  { path: '/settings/instance-mute', label: 'settings/instance-mute' },
  // games / fun
  { path: '/games', label: 'games' },
  { path: '/games/reversi', label: 'games/reversi' },
  // misc
  { path: '/announcements', label: 'announcements' },
  { path: '/scratchpad', label: 'scratchpad' },
  { path: '/lookup', label: 'lookup' },
  // admin (root user は admin role 持ち)
  { path: '/admin', label: 'admin (root)' },
  { path: '/admin/overview', label: 'admin/overview' },
  { path: '/admin/users', label: 'admin/users' },
  { path: '/admin/emojis', label: 'admin/emojis' },
  { path: '/admin/job-queue', label: 'admin/job-queue' },
  { path: '/admin/federation', label: 'admin/federation' },
  { path: '/admin/abuses', label: 'admin/abuses' },
  { path: '/admin/announcements', label: 'admin/announcements' },
  { path: '/admin/ads', label: 'admin/ads' },
  { path: '/admin/roles', label: 'admin/roles' },
];

test.describe('UI: authenticated SPA route matrix', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  // 36 route × signin の全 chain が serial 実行される想定。default 30s では
  // 足りないので spec 単位で延長する。各 case の navigation は最大 15-20s。
  test.setTimeout(180_000);

  // signin を全 case で 1 回だけ走らせて時間効率を取る。各 route navigation
  // は test.step() で wrap して Playwright の HTML report 上で個別 step
  // として可視化する (= 1 route 失敗時に他が pass 扱いになるわけではないが、
  // どの route が原因かを step trace で 1 jump で特定できる)。
  test('walk all authenticated routes and verify navbar persists', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);

    for (const route of AUTH_ROUTES) {
      await test.step(`navigate ${route.path} (${route.label})`, async () => {
        const resp = await page.goto(`${baseURL}${route.path}`, { waitUntil: 'domcontentloaded' });
        expect(resp, `goto ${route.path} returned null response`).not.toBeNull();
        expect(resp!.status(), `${route.label} should return 200 (SPA fallback)`).toBe(200);

        // hydration 完了確認: navbar の post button が visible (= authenticated
        // state 維持 + Vue mount 成功)
        await expect(
          page.locator('[data-testid="open-post-form"]').first(),
          `${route.label} should hydrate with authenticated navbar`,
        ).toBeVisible({ timeout: 20_000 });
      });
    }
  });
});
