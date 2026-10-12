/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/invites page で admin/invite/create で発行した invite code が
// MkInviteCode コンポ経由で render されることを verify する spec。
//
// /admin/invites は admin/invite/list を Paginator で叩いて invite を
// 一覧 render する。MkInviteCode は invite.code を <div :class="_selectableAtomic">
// として render するので body 検索可。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/invites renders newly issued invite codes', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('admin/invite/create + /admin/invites renders the new code', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1 件発行する (= count default 1)
    const createResp = await callApi(request, 'admin/invite/create', {
      i: root.token,
      count: 1,
    });
    expect(createResp.status()).toBe(200);
    const tickets = await createResp.json();
    expect(Array.isArray(tickets), 'admin/invite/create returns array').toBe(true);
    expect(tickets.length).toBeGreaterThanOrEqual(1);
    const code: string = tickets[0].code;
    expect(typeof code).toBe('string');
    expect(code.length).toBeGreaterThan(0);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/invites`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (c) => document.body.textContent?.includes(c) ?? false,
      code,
      { timeout: 20_000 },
    );
  });
});
