/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ユーザー検索 (/search?type=user) で、打った語が users/search に送られ、該当する
// 利用者だけが一覧に出て、選ぶとその利用者のページへ移ることを確かめる (#4049)。
//
// 一覧に出たことは、ページの本体の中の表示名で見る (サインイン中の自分の名前は
// ナビにも出るので、検索する人とは別の利用者を探す)。移った先は、URL と、ページが
// 引いた users/show の username で見る。

import { randomBytes } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { pageBody } from '../../../fixtures/route_check';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText } from '../../../fixtures/ui_click';
import { waitForApiBody } from '../../../fixtures/ui_post_form';
import { coversRoutes, expectRoutedPage, gotoRoute } from '../../../fixtures/ui_route';

test.describe('UI: user search opens the profile', () => {
  test.setTimeout(90_000);

  test('results list the matching users and choosing one opens their page', coversRoutes('/search'), async ({ page, baseURL, request }) => {
    const searcher = await signupUser(request, randomUsername('pwusr'));
    // 2 人だけが同じ接頭辞を持つ。もう 1 人は持たない
    const prefix = `us${randomBytes(3).toString('hex')}`;
    const first = await signupUser(request, randomUsername(prefix));
    const second = await signupUser(request, randomUsername(prefix));
    const outsider = await signupUser(request, randomUsername('pwusrno'));
    const names = new Map<string, string>();
    for (const u of [first, second, outsider]) {
      const name = `PW ${u.username}`;
      names.set(u.id, name);
      const r = await callApi(request, 'i/update', { i: u.token, name });
      expect(r.status()).toBe(200);
    }

    await uiSigninAs(page, request, baseURL!, searcher);
    await gotoRoute(page, baseURL!, '/search?type=user');
    const body = pageBody(page);
    const query = body.locator('input[type="search"]');
    await expect(query).toHaveCount(1, { timeout: 15_000 });
    await query.fill(prefix);

    const sent = waitForApiBody(page, 'users/search');
    const resp = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/users/search', { timeout: 15_000 });
    await clickButtonByText(page, 'Search');
    expect((await sent).query).toBe(prefix);
    const found = ((await (await resp).json()) as { id: string }[]).map((u) => u.id);
    expect(new Set(found)).toEqual(new Set([first.id, second.id]));

    await expect(body.getByText(names.get(first.id)!, { exact: true })).toBeVisible({ timeout: 15_000 });
    await expect(body.getByText(names.get(second.id)!, { exact: true })).toBeVisible();
    await expect(body.getByText(names.get(outsider.id)!, { exact: true })).toHaveCount(0);

    // 2 人目を選ぶと、その人のページへ移る
    const shown = waitForApiBody(page, 'users/show');
    await body.getByText(names.get(second.id)!, { exact: true }).click();
    expect((await shown).username).toBe(second.username);
    await expect(page).toHaveURL(new RegExp(`/@${second.username}$`), { timeout: 15_000 });
    await expectRoutedPage(page, `/@${second.username}`);
    await expect(pageBody(page).getByText(`@${second.username}`, { exact: false }).first()).toBeVisible({ timeout: 15_000 });
  });
});
