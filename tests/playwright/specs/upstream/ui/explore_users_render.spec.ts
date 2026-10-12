/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /explore#users page で recently-registered な user が render されることを
// verify する spec。
//
// upstream Misskey の router 定義では `/explore` 単体に `hash: 'initialTab'`
// が紐づいており、URL hash (= `#users`) が initialTab prop に渡って users
// tab で初期化される。`/explore/users` は SPA route として存在せず not-found
// に流れるので、本 spec は `/explore#users` を使う。
//
// signupUser でフレッシュ user を作成した直後に navigate すると、
// "Recently registered users" セクションに新 user の username が含まれる
// (= users/get-recently-registered endpoint からの hydration)。MkUserList
// + MkPagination の chain が壊れていないことの smoke。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, signupUser } from '../../../fixtures/auth';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /explore/users renders recently-registered users', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('newly signed-up user appears in /explore#users', async ({ page, baseURL, request }) => {
    const newUserName = `expnew${Date.now().toString().slice(-9)}`;
    const newUser = await signupUser(request, newUserName, DEFAULT_TEST_PASSWORD);
    // /explore#users の "recently registered" は users(state:'alive') を引く。
    // upstream の alive は `user.updatedAt > now-5d` なので、signup 直後の
    // updatedAt=NULL な user は比較が NULL になって一覧に出てこない
    // (endpoints/users.ts の `case 'alive'`)。upstream で local user の
    // updatedAt を立てるのは note 投稿時の incNotesCountOfUser だけなので
    // (i/update では立たない)、1 件投稿させて両 backend で出るようにする。
    // mk-go は updatedAt=NULL も含めるため mk-go 単体では通っていた (#2276)。
    const noteResp = await callApi(request, 'notes/create', {
      i: newUser.token,
      text: `hello from ${newUserName}`,
      visibility: 'public',
    });
    expect(noteResp.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/explore#users`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // /explore/users は recently-registered user を含む複数 section を
    // hydrate するので、新規 user の username が body に出るのを sign に
    // する (MkUserList の MkUserName 経由)。
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      newUserName,
      { timeout: 20_000 },
    );
  });
});
