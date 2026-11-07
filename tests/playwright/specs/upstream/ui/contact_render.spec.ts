/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /contact page (= 公開 maintainer info) で MkKeyValue + i18n.ts.contact
// (= "Contact") が hydrate されることを smoke する spec。anonymous access OK。

import { test } from '@playwright/test';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: /contact renders public maintainer info', () => {
  test.setTimeout(30_000);

  test('"Contact" label appears on /contact', coversRoutes('/contact'), async ({ page, baseURL }) => {
    await page.setViewportSize({ width: 1600, height: 900 });
    await gotoRoute(page, baseURL!, '/contact');

    // i18n.ts.contact → "Contact" label が MkKeyValue key として render
    // される。/contact 以外でも "Contact" は出ないので固有 sign。
    await page.waitForFunction(
      () => (document.body.textContent ?? '').includes('Contact'),
      { timeout: 20_000 },
    );
  });
});
