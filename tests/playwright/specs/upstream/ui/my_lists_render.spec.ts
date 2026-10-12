/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/lists page で users/lists/create で作成した list の name が
// MkA の link text として render されることを verify する spec。
//
// /my/lists は users/lists/list 経由で list 一覧を取得し、各 list を
// MkA + MkAvatars で render する。list.name + nUsers の i18n string が
// body に出るのを smoke する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /my/lists renders user-list names', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('users/lists/create + /my/lists renders list name', async ({ page, baseURL, request }) => {
    const listName = `pwlist-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'users/lists/create', {
      i: root.token,
      name: listName,
    });
    expect(createResp.status()).toBe(200);
    const list = await createResp.json();
    expect(list.id, 'users/lists/create should return id').toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/lists`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      listName,
      { timeout: 20_000 },
    );
  });
});
