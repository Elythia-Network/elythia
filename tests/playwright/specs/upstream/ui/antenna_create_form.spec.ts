/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/antennas/create で MkAntennaEditor の name input → Save click →
// /api/antennas/create round-trip → /my/antennas 一覧で新 antenna が
// render される **真の write-flow** spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { deleteAntennasNamed } from '../../../fixtures/quota';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /my/antennas/create form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  // 作った antenna は片付ける。root を共有しているので放置すると
  // antennaLimit (既定 5) を使い切って無関係な spec が create で落ちる (#2264)。
  const createdAntennas: string[] = [];
  test.afterEach(async ({ request }) => {
    await deleteAntennasNamed(request, root.token, createdAntennas);
    createdAntennas.length = 0;
  });

  test('navigate /my/antennas/create → fill name → save → list shows new antenna', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/my/antennas/create`, { waitUntil: 'domcontentloaded' });

    // name MkInput が hydrate するまで待つ
    await page.waitForFunction(
      () => document.querySelectorAll('input').length > 0,
      { timeout: 20_000 },
    );

    // name input (= 最初の input) に書き込み (post_note と同 pattern)
    const antennaName = `pwantui-${Date.now().toString().slice(-9)}`;
    createdAntennas.push(antennaName);
    await page.evaluate((n) => {
      const target = document.querySelector('input') as HTMLInputElement | null;
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, n);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, antennaName);

    // antennas/create は keywords か excludeKeywords のいずれかが必須
    // (upstream #14491 以降 EMPTY_KEYWORD を投げる)。エディタの最初の
    // textarea が "Keywords to listen to" なのでここを埋める。
    await page.evaluate(() => {
      const target = document.querySelector('textarea') as HTMLTextAreaElement | null;
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLTextAreaElement.prototype,
        'value',
      )?.set;
      setter?.call(target, 'pwkeyword');
      target.dispatchEvent(new Event('input', { bubbles: true }));
    });

    // antennas/create response を捕捉して Save click
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/antennas/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    await createResp;

    // SPA は /my/antennas に router.push する。新 antenna name が一覧に
    // 出るのを polling 待ち。
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      antennaName,
      { timeout: 20_000 },
    );
  });
});
