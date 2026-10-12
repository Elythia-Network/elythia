/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/security の "Log IP address" folder を expand → enableIpLogging
// switch を toggle → form footer の Save button click → /api/admin/update-meta
// が round-trip する write-flow spec。
//
// admin/security.vue:140-157 の ipLoggingForm は MkFolder 内で 1 個の
// MkSwitch を持つ。switch 変更で form.modified=true → footer の
// MkFormFooter (= Save button) が表示される。Save click で
// admin/update-meta が走る (line 211)。
//
// **checkbox を index で引かない。** 「他の folder は collapsed なので
// 1 個目が目的のもの」という前提は、同 page に switch が 1 つ増えるだけで
// 崩れる。しかも押す対象が変わっても spec は緑のままになる (#2620 で
// admin/moderation が実際にそうなっていた)。folder に scope して引き、
// 最後に meta の値で「意図した設定が変わったか」を verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /admin/security IP logging form save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('expand Log IP address folder → toggle switch → Save → /api/admin/update-meta', async ({
    page,
    baseURL,
    request,
  }) => {
    // setup: 既知 state (false) に reset。
    await callApi(request, 'admin/update-meta', {
      i: root.token,
      enableIpLogging: false,
    });

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/security`, {
        waitUntil: 'domcontentloaded',
      });

      // page hydrate を待つ — admin/security の folder header (data-cy-folder-header)
      // が複数 mount するまで。
      await page.waitForFunction(
        () => document.querySelectorAll('[data-testid="folder-header"]').length >= 3,
        { timeout: 20_000 },
      );

      // "Log IP address" を含む folder header を click して expand
      await clickWhenReady(page, '「Log IP address」の folder-header', () => {
        const headers = Array.from(
          document.querySelectorAll('[data-testid="folder-header"]'),
        ) as HTMLElement[];
        const target = headers.find((h) =>
          (h.textContent ?? '').includes('Log IP address'),
        );
        return target;
      });

      // checkbox を click → form.modified=true → footer の Save button 出現。
      // MkFolder の root は role="group" で header と本文の両方を含むので、
      // header の文言で folder を特定してからその中の switch を引く。
      await clickWhenReady(page, '「Log IP address」の Enable スイッチ', (l: string) => {
        const group = Array.from(document.querySelectorAll('[role="group"]')).find((g) =>
          (g.querySelector('[data-testid="folder-header"]')?.textContent ?? '').includes(l),
        );
        return group?.querySelector('input[type="checkbox"]');
      }, 'Log IP address');

      // footer の Save button hydrate を待つ
      await page.waitForFunction(
        () => {
          const btns = Array.from(document.querySelectorAll('button'));
          return btns.some((b) => (b.textContent ?? '').includes('Save'));
        },
        { timeout: 10_000 },
      );

      // Save click → admin/update-meta round-trip
      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/admin/update-meta') && r.status() < 300,
        { timeout: 15_000 },
      );
      await clickButtonContainingText(page, 'Save');
      await updateResp;

      // update-meta が返ってきたことだけを見ると、別の switch を押していても
      // 緑になる。実際に変わった field を meta で確かめる。
      const metaResp = await callApi(request, 'admin/meta', { i: root.token });
      expect(metaResp.status()).toBe(200);
      expect((await metaResp.json()).enableIpLogging).toBe(true);
    } finally {
      // cleanup: 必ず false に戻す。assert が落ちた経路でも通す。残ると
      // ログ容量を肥大させる。
      await callApi(request, 'admin/update-meta', {
        i: root.token,
        enableIpLogging: false,
      });
    }
  });
});
