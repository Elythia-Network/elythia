/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定のメニューの検索で項目を探して選ぶと、その項目のあるページへ移り、その項目に
// 印 (ハイライト) が付くことを確かめる (#4049)。
//
// 印は SearchMarker が付ける (アドレスの # に項目の id が入る)。印の付いた要素は
// data-testid="search-marker-highlighted" を持つ。検索する前 (# の無いとき) には
// 印が無いことも見る。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { pageBody } from '../../../fixtures/route_check';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

const LABEL = 'Sync Dark Mode with your device settings';

test.describe('UI: settings search', () => {
  test.setTimeout(90_000);

  test('a search result opens its page and highlights the item', coversRoutes('/settings/theme'), async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwsetsrch'));
    await uiSigninAs(page, request, baseURL!, user);

    // 検索する前は、印が無い
    await gotoRoute(page, baseURL!, '/settings/theme');
    await expect(pageBody(page).getByText(LABEL, { exact: true })).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId('search-marker-highlighted')).toHaveCount(0);

    await gotoRoute(page, baseURL!, '/settings/profile');
    const search = page.locator('[data-testid="super-menu-search"] input');
    await expect(search).toHaveCount(1, { timeout: 15_000 });
    await search.fill('sync dark mode');
    const results = page.getByTestId('super-menu-search-result');
    await expect(results.first()).toBeVisible({ timeout: 10_000 });
    // 結果は全部、検索した語に関わるもの (ほかの項目まで出ていない)
    const texts = await results.evaluateAll((els) => els.map((e) => (e.textContent ?? '').toLowerCase()));
    expect(texts.some((t) => t.includes(LABEL.toLowerCase()))).toBe(true);
    expect(texts.some((t) => t.includes('profile'))).toBe(false);

    await clickWhenReady(page, `検索の結果「${LABEL}」(ちょうど 1 つ)`, (l: string) => {
      const hits = Array.from(document.querySelectorAll('[data-testid="super-menu-search-result"]')).filter((e) => (e.textContent ?? '').includes(l));
      return hits.length === 1 ? hits[0] : null;
    }, LABEL);
    await expect(page).toHaveURL(/\/settings\/theme#.+/, { timeout: 15_000 });

    const marked = page.getByTestId('search-marker-highlighted');
    await expect(marked).toHaveCount(1, { timeout: 15_000 });
    await expect(marked).toContainText(LABEL);
    // 印の付いた項目は、ページの本体の中にある
    expect(await marked.evaluate((el) => el.closest('._pageContainer') !== null)).toBe(true);
  });
});
