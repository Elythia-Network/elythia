/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// frontend (Misskey TS SPA) を実 browser で開いて動作確認する smoke spec。
//
// 既存の spec は API shape (callApi) のみで browser を起動しないため、
// frontend asset 配信 / index.html / manifest / static asset path / SPA 起動が
// 壊れていても検出できなかった。本 spec は page.goto で `/` を navigate し、
// title / mount root / asset の応答 200 を確認することで「frontend が見える」
// 状態を回帰検出する。
//
// 注: 本 spec は最低限「frontend がロードされて Vue が mount できる」ところ
// までで、SPA 内の DOM 操作には踏み込まない。実 click を伴う UI 操作は
// data-cy-* selector 経由で specs/ui/*.spec.ts (signin / post_note /
// content_pages_extra 等) に分離した。なお drop-in 切替シナリオ (= TS から
// mk-go へ DB を引き継いで切替) を視点にした視覚回帰は cypress
// (`tests/dropin-frontend/`) 側で別途 cover している。

import { expect, test } from '@playwright/test';
import { isTsBackend } from '../../../../fixtures/backend';

test.describe('smoke: frontend SPA loads', () => {
  test('GET / returns index.html with root mount node', async ({ page, baseURL }) => {
    const resp = await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });
    expect(resp).not.toBeNull();
    expect(resp!.status()).toBe(200);

    // SPA の mount root (Misskey は #app / #misskey_app に Vue を mount)。
    // domcontentloaded だけだと loader spinner だけが render された状態で
    // pass してしまい (動画上は真っ白の loading 画面)、実 SPA boot を
    // verify できない。Vue mount 完了の兆候 (mount root に child element /
    // interactive element の出現) を待って "実 content がある" 状態を確認。
    await page.waitForFunction(
      () => {
        const app = document.querySelector('#app, #misskey_app');
        if (app && app.children.length > 0) return true;
        // SPA boot 中に main / nav / button が hydrate していれば OK
        return (
          document.querySelector('main, nav, button[type], a[href]:not([href=""])') !== null
        );
      },
      { timeout: 20_000 },
    );

    const title = await page.title();
    expect(title.length).toBeGreaterThan(0);

    // body 内 DOM が hydrate 済みの content を含むことを再確認。
    const bodyHTML = await page.evaluate(() => document.body.innerHTML);
    expect(bodyHTML.length).toBeGreaterThan(50);
  });

  // splash 画面の DOM 構造が upstream `_splash.tsx` 互換であること (#993)。
  // page.goto だと Vue mount で splash が消えるので、raw HTML を fetch して
  // initial response の markup を verify する。
  test('initial HTML contains upstream-compatible splash markup', async ({ request, baseURL }) => {
    const resp = await request.get(`${baseURL}/`);
    expect(resp.status()).toBe(200);
    const html = await resp.text();
    // splash 構造の必須要素
    expect(html).toContain('<div id="splash">');
    expect(html).toContain('<img id="splashIcon"');
    expect(html).toContain('<div id="splashSpinner">');
    // ここから下は mk-go 独自のスプラッシュ。TS baseline は公式の HTML を返す。
    if (isTsBackend) return;
    // スピナーは mk-go 独自 (#2549)。回転する層 (.rig) と半径方向に動く
    // 点 (.pkt) を分けてある — 1 つの要素で両方やらせると transform が
    // 衝突して、集まる動きが回転に巻き取られる。
    expect(html).toContain('class="rig"');
    expect(html).toContain('class="pkt p1"');
    expect(html).toContain('class="pkt p6"');
    // 色はサーバー設定のテーマカラーを渡す。--MI_THEME-accent は利用者が
    // 選んだテーマの色なので、テーマ適用前に出るスプラッシュには使えない。
    expect(html).toContain('--splash-color:');
    // 旧 mascot (ai.png) や placeholder text は出ないこと
    expect(html).not.toContain('src="/assets/ai.png"');
    expect(html).not.toContain('<p>Loading...</p>');
  });

  test('frontend asset (manifest.json) is served', async ({ request, baseURL }) => {
    // Vite manifest が frontend ビルドから配信されているかを確認。manifest
    // が無いと SPA は起動できない。注: `/_frontend_vite_/manifest.json` は
    // Vite asset manifest、`/manifest.json` は PWA manifest で別物。SPA
    // fallback router は manifest.json 経路に index.html を返してくる
    // ことがある (= 200 + HTML body) ので、JSON parse + object 判定まで
    // 通った時点で「真の manifest が配信されている」と判定する。
    const candidates = [
      `${baseURL}/_frontend_vite_/manifest.json`,
      `${baseURL}/manifest.json`,
    ];
    type ManifestResult = { url: string; body: Record<string, unknown> };
    let result: ManifestResult | null = null;
    const attempts: string[] = [];
    for (const url of candidates) {
      const resp = await request.get(url);
      if (resp.status() !== 200) {
        attempts.push(`${url} → status ${resp.status()}`);
        continue;
      }
      const body = await resp.text();
      let parsed: unknown;
      try {
        parsed = JSON.parse(body);
      } catch {
        attempts.push(`${url} → 200 but not JSON (HTML fallback?)`);
        continue;
      }
      if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        attempts.push(`${url} → 200 + JSON but not an object`);
        continue;
      }
      result = { url, body: parsed as Record<string, unknown> };
      break;
    }
    expect(
      result,
      `no manifest.json candidate returned a JSON object. tried:\n  ${attempts.join('\n  ')}`,
    ).not.toBeNull();
  });
});
