/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /about page (= 公開 instance about ページ) で instance.name + i18n
// 文字列が hydrate されることを verify する spec。
//
// /about は overview / emojis / federation / charts の 4 tab 構成で
// default は overview。overview では `<b>{{ instance.name }}</b>` を render
// するので、test 環境の instance host (= mkgo.local) が body に出るのを
// hydration sign にする。

import { expect, test } from '@playwright/test';

test.describe('UI: /about renders public instance overview', () => {
  test.setTimeout(30_000);

  test('instance host appears in /about overview tab', async ({ page, baseURL }) => {
    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/about`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // instance.name は admin/update-meta で未設定なら null になり host
    // (= mkgo.local) で fallback される。test 環境の baseURL host を
    // body 検索するのが最も移植性が高い hydration sign。port 付きの
    // baseURL でも body には hostname のみが出るので hostname (port 抜き)
    // を使う。
    const hostname = new URL(baseURL!).hostname;
    await page.waitForFunction(
      (h) => document.body.textContent?.includes(h) ?? false,
      hostname,
      { timeout: 20_000 },
    );
  });
});
