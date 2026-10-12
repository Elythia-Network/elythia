/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/moderation の "Blocked hosts" MkFolder を expand → MkTextarea
// を編集 → Save → /api/admin/update-meta が round-trip する write-flow
// spec。preserved_usernames / prohibited_words の sister。
//
// blocked hosts は federation 系で domain ブロックリスト。upstream key は
// `blockedHosts` で、admin/update-meta の同 field 経由で persist される。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /admin/moderation blockedHosts save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('expand Blocked hosts folder → edit textarea → Save → /api/admin/update-meta', async ({
    page,
    baseURL,
    request,
  }) => {
    // setup: 既知 state (空 list) に reset。
    await callApi(request, 'admin/update-meta', {
      i: root.token,
      blockedHosts: [],
    });

    try {
      await uiSigninAsRoot(page, baseURL, root);
      await page.goto(`${baseURL}/admin/moderation`, {
        waitUntil: 'domcontentloaded',
      });

      // 「`folder header が >= 5 個` という count 条件で待つと、folder が遅延
      // hydrate するインスタンスで稀に 20s 超えて 60s test timeout に達する
      // 回帰がある (実機観察)。本来 spec が必要なのは "Blocked hosts" header
      // の存在だけなので、count ではなく target text を含む header の存在を
      // 直接待つ条件に切替える。folder 数が将来増減しても影響を受けない。
      await page.waitForFunction(
        () => {
          const headers = Array.from(
            document.querySelectorAll('[data-testid="folder-header"]'),
          ) as HTMLElement[];
          return headers.some((h) =>
            (h.textContent ?? '').includes('Blocked Instances'),
          );
        },
        { timeout: 30_000 },
      );

      // "Blocked hosts" folder を expand
      await clickWhenReady(page, '「Blocked Instances」の folder-header', () => {
        const headers = Array.from(
          document.querySelectorAll('[data-testid="folder-header"]'),
        ) as HTMLElement[];
        const target = headers.find((h) =>
          (h.textContent ?? '').includes('Blocked Instances'),
        );
        return target;
      });

      await page.waitForFunction(
        () => document.querySelectorAll('textarea').length >= 1,
        { timeout: 10_000 },
      );

      const newValue = `pwblock-${Date.now().toString().slice(-9)}.invalid\nfoo.invalid\nbar.invalid`;
      await page.evaluate((v) => {
        const tas = Array.from(document.querySelectorAll('textarea')) as HTMLTextAreaElement[];
        const target = tas[0];
        if (!target) return;
        target.focus();
        const setter = Object.getOwnPropertyDescriptor(
          window.HTMLTextAreaElement.prototype,
          'value',
        )?.set;
        setter?.call(target, v);
        target.dispatchEvent(new Event('input', { bubbles: true }));
      }, newValue);

      const updateResp = page.waitForResponse(
        (r) => r.url().includes('/api/admin/update-meta') && r.status() < 300,
        { timeout: 15_000 },
      );
      await clickWhenReady(page, '「Save」のボタン', () =>
        Array.from(document.querySelectorAll('button')).find(
          (b) => !b.disabled && (b.textContent ?? '').includes('Save'),
        ),
      );
      await updateResp;
    } finally {
      // cleanup: blockedHosts に test domain (.invalid) が残っても他 spec
      // への直接影響は無いが、test isolation のため空に戻す。
      await callApi(request, 'admin/update-meta', {
        i: root.token,
        blockedHosts: [],
      });
    }
  });
});
