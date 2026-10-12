/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/apps (認可済みアプリの一覧) をブラウザで操作する (#2441)。
//
// **この画面はトークンを失効させる唯一の入口**。アプリに渡したトークンが漏れた
// とき、利用者がここから revoke できないと権限を取り上げる手段が無くなる。
// 一覧が出ない / 削除ボタンが効かないという壊れ方は、API が 200 を返していても
// 起きるうえ、平時は誰も開かないので気付かれにくい。
//
// トークンは `/auth/:token` の UI を経由せず API だけで用意する。
//
//   app/create           → アプリ (secret 付き)
//   auth/session/generate → 認可セッション
//   auth/accept          → 本人が承認 (フロントの /auth/:token が呼ぶのと同じ)
//   auth/session/userkey  → アクセストークンが materialize される
//
// `/auth/:token` の画面自体は `auth_authorize.spec.ts` が別途見ているので、
// ここでは重複させずに一覧と失効に集中する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

interface AuthorizedApp {
  name: string;
  accessToken: string;
}

/** Authorize a freshly created app for root and materialize its access token. */
async function authorizeApp(
  request: import('@playwright/test').APIRequestContext,
  root: RootFixture,
): Promise<AuthorizedApp> {
  const name = `pw-app-${Date.now().toString().slice(-9)}`;
  const appResp = await callApi(request, 'app/create', {
    name,
    description: 'playwright fixture',
    permission: ['read:account'],
  });
  expect(appResp.status()).toBe(200);
  const app = (await appResp.json()) as { secret: string };

  const sessionResp = await callApi(request, 'auth/session/generate', { appSecret: app.secret });
  expect(sessionResp.status()).toBe(200);
  const session = (await sessionResp.json()) as { token: string };

  const accepted = await callApi(request, 'auth/accept', { i: root.token, token: session.token });
  expect(accepted.status()).toBe(204);

  const userkey = await callApi(request, 'auth/session/userkey', {
    appSecret: app.secret,
    token: session.token,
  });
  expect(userkey.status()).toBe(200);
  const { accessToken } = (await userkey.json()) as { accessToken: string };

  return { name, accessToken };
}

test.describe('UI: /settings/apps authorized applications', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('認可したアプリが一覧に表示される', async ({ page, baseURL, request }) => {
    const app = await authorizeApp(request, root);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/apps`, { waitUntil: 'domcontentloaded' });

    // どのアプリに何を許可したのか分からないと、失効の判断ができない。
    await expect(page.getByText(app.name, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('削除を押すとトークンが失効する', async ({ page, baseURL, request }) => {
    const app = await authorizeApp(request, root);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/apps`, { waitUntil: 'domcontentloaded' });
    await expect(page.getByText(app.name, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });

    // 対象アプリの MkFolder 内にある Delete を押す。他のアプリの行を巻き込まない
    // よう、アプリ名と Delete の両方を含む最も内側の要素から辿る。
    const card = page
      .locator('div')
      .filter({ has: page.getByText(app.name, { exact: false }) })
      .filter({ has: page.getByRole('button', { name: 'Delete' }) })
      .last();

    const revoked = page.waitForResponse(
      (r) => r.url().includes('/api/i/revoke-token') && r.status() < 400,
      { timeout: 20_000 },
    );
    await card.getByRole('button', { name: 'Delete' }).first().click();
    await revoked;

    // **失効の本体はトークンが使えなくなること。** 一覧から消えるだけでは
    // 権限は残ったままになりうる。
    await expect(async () => {
      const me = await callApi(request, 'i', { i: app.accessToken });
      expect(me.status()).toBeGreaterThanOrEqual(400);
    }).toPass({ timeout: 15_000 });
  });
});
