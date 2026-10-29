/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/federation の Deliver / Inbox タブが mk-go 独自の
// admin/federation/{delivery,inbox}-health を叩いて描画されることを verify する
// spec (#2944)。
//
// **配送実績の有無に依存させない。** テスト環境では連合が起きていないので
// hosts は空で返る。検証するのは「本物の endpoint が応答してビューが立ち上がる」
// ところまでで、ホストの中身までは見ない。
//
// **DOM だけを見てはいけない。** summary は fetch の完了を待たずに描かれるので、
// 文字列の有無だけを条件にすると **misskeyApi の呼び出しを丸ごと消しても通る**
// (敵対的レビューで実測された)。タブを押した時点で飛ぶリクエストを
// waitForResponse で捕まえる。
//
// **200 だけでも足りない。** mk-go の API catchall は未登録の POST に
// 200 + `{}` を返す (`internal/server/plugin_peer.go` — 404 にすると公式フロントの
// 一部ページが例外を投げるための意図的な pass-through)。したがって endpoint 名を
// typo しても、**backend のルート登録を消しても** 200 が返り、`hosts ?? []` で
// 空表示のまま緑になる。pathname を完全一致で見たうえで、**応答の形**
// (windowSeconds が数値 / hosts が配列) まで確認して catchall と区別する。
//
// locale は en-US で確定する (`packages/frontend-shared/js/config.ts` の
// `localStorage.getItem('lang') ?? 'en-US'` で、lang を書くのは設定ページだけ)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

const UNAVAILABLE = 'Could not load delivery health';

// タブの表示名と、押したときに飛ぶ endpoint。タブ名は連合ジョブの画面と
// 揃えた固定文字列 (i18n を通さない)。
const HEALTH_TABS = [
  { label: 'Deliver', endpoint: 'admin/federation/delivery-health' },
  { label: 'Inbox', endpoint: 'admin/federation/inbox-health' },
];

test.describe('UI: /admin/federation delivery health tabs', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(90_000);

  test('Deliver / Inbox tabs fetch and render the mk-go health endpoints', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/federation`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('Deliver') && text.includes('Inbox');
      },
      { timeout: 20_000 },
    );

    for (const { label, endpoint } of HEALTH_TABS) {
      // クリックで飛ぶリクエストを捕まえる。ここが本体で、DOM の確認は
      // そのあとの hydration を見るだけ。
      const [apiResp] = await Promise.all([
        page.waitForResponse(
          (r) => new URL(r.url()).pathname === `/api/${endpoint}` && r.request().method() === 'POST',
          { timeout: 20_000 },
        ),
        page.getByText(label, { exact: true }).first().click(),
      ]);
      expect(apiResp.status()).toBe(200);

      // catchall は `{}` を返すので、ここが本物の endpoint かどうかの判別になる。
      // windowSeconds は telemetry 未配線の構成でも必ず入る。
      const body = await apiResp.json();
      expect(typeof body.windowSeconds).toBe('number');
      expect(Array.isArray(body.hosts)).toBe(true);

      // 200 が返っても、描画側が unavailable に倒れていれば配線は不完全。
      await page.waitForFunction(
        (unavailable) => {
          const text = document.body.textContent ?? '';
          return text.includes('Success rate') && !text.includes(unavailable);
        },
        UNAVAILABLE,
        { timeout: 20_000 },
      );
    }

    // インスタンス一覧へ戻れること。タブを足したことで既存のビューが出なく
    // なっていないかを見る。**両方向を見る必要がある** — "Host" 単独では健全性
    // タブの summary ("Hosts") に部分一致して切り替え前から真になり、逆に
    // "Success rate" が消えたことだけでは instances ブランチが空になる回帰を
    // 見逃す。
    await page.getByText('Instances', { exact: true }).first().click();
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return !text.includes('Success rate') && text.includes('Host');
      },
      { timeout: 20_000 },
    );
  });
});
