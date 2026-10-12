/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/webhook/new で name + url + secret MkInput → Create click →
// /api/i/webhooks/create round-trip → SPA は dialog 後に router.back する
// (apiWithDialog 経由)。**真の write-flow** spec。
//
// 注意: /settings/* は親 layout (settings/index.vue) に MkSuperMenu 内蔵の
// 検索 MkInput (type="search") があり、page 全体の input[0] はこの search
// box になる。webhook form の name / url / secret を取るには
// type="search" を skip して filter する必要がある (#744 batch3 で発覚)。
// 7 switch は全て default true。Create button は textContent "Create"。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { deleteWebhooksNamed } from '../../../fixtures/quota';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /settings/webhook/new form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  // 作った webhook は片付ける。root を共有しているので放置すると
  // webhookLimit (既定 3) を使い切って無関係な spec が create で落ちる (#2264)。
  const createdWebhooks: string[] = [];
  test.afterEach(async ({ request }) => {
    await deleteWebhooksNamed(request, root.token, createdWebhooks);
    createdWebhooks.length = 0;
  });

  test('navigate /settings/webhook/new → fill name+url+secret → Create → i/webhooks/create round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/webhook/new`, { waitUntil: 'domcontentloaded' });

    // settings 親 layout の search input を除く form 本体の input が
    // 3 個 hydrate (name / url / secret)。
    await page.waitForFunction(
      () => {
        const inputs = Array.from(
          document.querySelectorAll('input'),
        ) as HTMLInputElement[];
        return inputs.filter((i) => i.type !== 'search').length >= 3;
      },
      { timeout: 20_000 },
    );

    const webhookName = `pwwhui-${Date.now().toString().slice(-9)}`;
    createdWebhooks.push(webhookName);
    const webhookUrl = 'https://example.test/webhook';
    const webhookSecret = 'pwwhui-secret';

    await page.evaluate(
      ({ n, u, s }) => {
        // SuperMenu の type="search" を skip し form 本体の input のみ取る。
        const inputs = (
          Array.from(document.querySelectorAll('input')) as HTMLInputElement[]
        ).filter((i) => i.type !== 'search');
        const setter = Object.getOwnPropertyDescriptor(
          window.HTMLInputElement.prototype,
          'value',
        )?.set;
        const setValue = (el: HTMLInputElement, v: string) => {
          el.focus();
          setter?.call(el, v);
          el.dispatchEvent(new Event('input', { bubbles: true }));
        };
        if (inputs[0]) setValue(inputs[0], n);
        if (inputs[1]) setValue(inputs[1], u);
        if (inputs[2]) setValue(inputs[2], s);
      },
      { n: webhookName, u: webhookUrl, s: webhookSecret },
    );

    // i/webhooks/create response 捕捉して Create click
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/webhooks/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Create');
    const created = await createResp;
    const body = await created.json();
    expect(body.id).toBeTruthy();
    expect(body.name).toBe(webhookName);
    expect(body.url).toBe(webhookUrl);
  });
});
