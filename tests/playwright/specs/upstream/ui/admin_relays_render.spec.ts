/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/relays page で admin/relays/add で登録した relay inbox URL が
// MkButton 行の text として render されることを verify する spec。
//
// /admin/relays は admin/relays/list で relay 一覧を取得し、各 relay の
// inbox URL + status を panel として render する。本 spec は inbox URL
// が body に出るのを hydration sign にする。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/relays page renders configured relays', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('admin/relays/add + /admin/relays renders relay inbox URL', async ({
    page,
    baseURL,
    request,
  }) => {
    // テスト用 relay inbox URL (= 実 federation はしないので適当な host で OK)
    const inboxUrl = `https://pwrelay-${Date.now().toString().slice(-9)}.invalid/inbox`;
    const addResp = await callApi(request, 'admin/relays/add', {
      i: root.token,
      inbox: inboxUrl,
    });
    expect(addResp.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/relays`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (u) => document.body.textContent?.includes(u) ?? false,
      inboxUrl,
      { timeout: 20_000 },
    );
  });
});
