/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/overview page で MkFoldableSection の各 section header
// (Stats / Active users / Heatmap / Retention rate / Moderators 等) が
// hydrate されることを smoke する spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/overview page hydrates dashboard sections', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('overview sections (Stats / Active users / Moderators) appear', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/overview`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // 各 MkFoldableSection の header text は upstream で英文 hardcode。
    // 3 つ以上の section header が body に出れば overview 本体の hydration
    // 完了と判定 (= home dashboard 等の他 page で同 3 文字列が同時に出る
    // ことは無い)。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return (
          text.includes('Stats') &&
          text.includes('Active users') &&
          text.includes('Moderators')
        );
      },
      { timeout: 20_000 },
    );
  });
});
