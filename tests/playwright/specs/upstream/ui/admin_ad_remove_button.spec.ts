/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/ads で 該当 ad container の Remove button (ti-trash + "Remove")
// click → confirm OK → /api/admin/ad/delete round-trip する write-flow
// spec。
//
// admin/ads.vue:76 の MkButton danger は ti-trash + "Remove" text。click
// すると os.confirm warning → 承諾後 admin/ad/delete を叩く (line 187)。
// admin_ad_create_form spec の inverse として、remove path を strict 化。
// 同 page には複数 ad container が存在し得るので、ad の URL を含む
// container 内の Remove button を選択する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /admin/ads remove button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('Remove button → confirm OK → /api/admin/ad/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 ad を API で create
    const url = `https://pwad-${Date.now().toString().slice(-9)}.invalid`;
    const createResp = await callApi(request, 'admin/ad/create', {
      i: root.token,
      url,
      imageUrl: `${url}/img.png`,
      memo: 'pw-ad-remove',
      place: 'square',
      priority: 'middle',
      ratio: 1,
      expiresAt: Date.now() + 365 * 24 * 60 * 60 * 1000,
      startsAt: Date.now(),
      dayOfWeek: 0,
    });
    expect(createResp.status()).toBeLessThan(400);

    // 2. /admin/ads を開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/ads`, {
      waitUntil: 'domcontentloaded',
    });

    // ad URL が body に出るまで待つ (= list 反映)
    await page.waitForFunction(
      (u) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === u);
      },
      url,
      { timeout: 20_000 },
    );

    // 3. 該当 ad container 内の Remove button click
    // ad の URL input から DOM 上方に遡って section を見つけ、その中の
    // ti-trash button を click する。多数 ad があっても URL 一致で識別可能。
    await page.evaluate((u) => {
      const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
      const target = inputs.find((i) => i.value === u);
      if (!target) return;
      // 上方に section の root を辿る (= ad container)。十分深い limit。
      let node: HTMLElement | null = target;
      for (let depth = 0; depth < 10 && node; depth++) {
        const btn = Array.from(node.querySelectorAll('button')).find(
          (b) => b.querySelector('i.ti-trash') !== null,
        );
        if (btn) {
          (btn as HTMLButtonElement).click();
          return;
        }
        node = node.parentElement;
      }
    }, url);

    // 4. confirm dialog OK click
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    const deleteResp = page.waitForResponse(
      (r) => r.url().includes('/api/admin/ad/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await deleteResp;

    // 5. API 経由で削除確認
    const listResp = await callApi(request, 'admin/ad/list', {
      i: root.token,
      limit: 100,
    });
    expect(listResp.status()).toBe(200);
    const list = await listResp.json();
    const found = list.find((a: { url: string }) => a.url === url);
    expect(found).toBeUndefined();
  });
});
