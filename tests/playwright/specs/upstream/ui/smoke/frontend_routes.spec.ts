/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// SPA route navigation smoke (public routes)。
//
// frontend SPA の主要 public route を実 browser で navigate して、index.html
// fallback / SPA router boot / vite chunk 読み込みが壊れていないことを回帰
//検出する。authenticated routes (= /my、/notifications、/settings 等) は
// IndexedDB 経由の auth state injection が必要で、本 spec の scope 外
// (= 後続 PR で `support/auth.ts` ヘルパー追加後に対応)。
//
// 各 test の前提:
// - public route なので未認証で 200 を返す
// - SPA は SSR を持たないため最初は loader だけが render され、後続で Vue
//   が mount される。ここでは waitUntil: 'domcontentloaded' で loader が
//   見える状態までで OK
// - 未知 route も SPA fallback で index.html が返る (= status 200)。これは
//   Vue Router 側でまた別の "not found" 表示にする想定なので status は 200
//   でも問題なし

import { expect, test } from '@playwright/test';

const PUBLIC_ROUTES = [
  { path: '/', label: 'home (logged-out fallback)' },
  { path: '/about', label: 'about' },
  { path: '/about-misskey', label: 'about-misskey (intro)' },
  { path: '/contact', label: 'contact' },
  { path: '/explore', label: 'explore' },
  { path: '/explore/featured', label: 'explore/featured' },
  { path: '/featured', label: 'featured timeline' },
  { path: '/login', label: 'login form' },
  { path: '/signup', label: 'signup form' },
];

test.describe('smoke: SPA public route navigation', () => {
  for (const { path, label } of PUBLIC_ROUTES) {
    test(`navigate ${path} (${label})`, async ({ page, baseURL }) => {
      const resp = await page.goto(`${baseURL}${path}`, { waitUntil: 'domcontentloaded' });
      expect(resp, `goto ${path} returned null response`).not.toBeNull();
      // SPA は index.html を返すので 200 (= mk-go の frontend.go fallback も同じ挙動)
      expect(resp!.status(), `${path} should return 200 (SPA fallback)`).toBe(200);

      // Vue mount 完了まで待つ。domcontentloaded の時点では loader spinner
      // しか出ていないので、それを pass 条件にすると動画上は真っ白の
      // loading 画面のまま完了してしまう (= 実 SPA は動作確認できていない)。
      // mount root の child / 主要 interactive element の出現を待って
      // 実 content がある状態を verify する。
      await page.waitForFunction(
        () => {
          const app = document.querySelector('#app, #misskey_app');
          if (app && app.children.length > 0) return true;
          return (
            document.querySelector('main, nav, button[type], a[href]:not([href=""])') !== null
          );
        },
        { timeout: 20_000 },
      );

      const title = await page.title();
      expect(title.length, `${path} should have non-empty <title>`).toBeGreaterThan(0);

      // body 内 DOM が hydrate 済 content を含むことを再確認。
      const bodyHTML = await page.evaluate(() => document.body.innerHTML);
      expect(bodyHTML.length, `${path} body should be > 50 chars (hydrated)`).toBeGreaterThan(50);
    });
  }
});

test.describe('smoke: SPA asset chunk health', () => {
  test('main bootstrap chunk is reachable from index.html', async ({ page, baseURL }) => {
    // network 上で 4xx/5xx を返した chunk を集計する。frontend が boot 段階で
    // 4xx を返すのは vite manifest と backend asset router の不整合 (= deploy
    // 事故) を即座に発見できる。
    const failures: { url: string; status: number }[] = [];
    page.on('response', (resp) => {
      const status = resp.status();
      // 304 も 4xx に含まれない、3xx は redirect で正常
      if (status >= 400) {
        failures.push({ url: resp.url(), status });
      }
    });

    // ホームを open。SPA 内部 chunk が遅延読み込みされるので networkidle
    // まで待つ。10s で十分 (slowest chunk は通常 < 3s)。
    await page.goto(`${baseURL}/`, { waitUntil: 'networkidle', timeout: 10_000 });

    // /api/ 系の 4xx (= 未認証の users/show 等) は SPA boot の挙動として
    // 想定内。frontend asset (`/assets/`, `/_frontend_vite_/`, `/static-assets/`,
    // `/twemoji/`, `/identicon/` 等) の 4xx だけを fail とみなす。
    const assetFailures = failures.filter((f) => {
      const u = new URL(f.url);
      return (
        u.pathname.startsWith('/_frontend_vite_/') ||
        u.pathname.startsWith('/static-assets/') ||
        u.pathname.startsWith('/twemoji/') ||
        u.pathname.startsWith('/assets/')
      );
    });
    expect(
      assetFailures,
      `frontend asset chunks should not 4xx/5xx, got: ${assetFailures.map((f) => `${f.status} ${f.url}`).join(', ')}`,
    ).toEqual([]);
  });
});
