/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 外部アプリ連携の承認画面 (/auth/:token) を実ブラウザで操作する (#2441)。
//
// API レイヤは `specs/upstream/api/app/auth_flow.spec.ts` が検証済みだが、
// そちらは `auth/accept` を API で直接叩いており、**承認画面を人が操作する経路は
// 未検証**だった。同 spec のコメントにもそう書いてある:
//
//   > url は user が踏む承認 URL (= /auth/<token>)。frontend test では
//   > Playwright の page.goto で承認 UI を踏む経路を取るが、本 spec は
//   > backend API レイヤのみなので存在のみ確認
//
// ここが壊れると **外部アプリが 1 つも連携できなくなる**。API が 200 を返していても
// 画面のボタンが動かなければ利用者は先に進めないので、UI 側の検証が要る。
//
// ボタンは `data-testid` を持たないのでテキストで特定する。playwright.config.ts が
// browser locale を en-US に固定しているため `Accept` / `Cancel` で安定する
// (vite の hash class は production ビルドで変わるので使わない)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

type App = { id: string; secret: string };
type Session = { token: string; url: string };

/** app を作って認可セッションを開始し、承認画面の URL を返す。 */
async function startAuthSession(
  request: Parameters<typeof callApi>[0],
  permissions: string[] = ['read:account'],
): Promise<{ app: App; session: Session }> {
  const createResp = await callApi(request, 'app/create', {
    name: `pw-auth-${Date.now()}`,
    description: 'playwright ui auth spec',
    permission: permissions,
  });
  expect(createResp.status()).toBe(200);
  const app = (await createResp.json()) as App;

  const sessionResp = await callApi(request, 'auth/session/generate', {
    appSecret: app.secret,
  });
  expect(sessionResp.status()).toBe(200);
  const session = (await sessionResp.json()) as Session;
  return { app, session };
}

test.describe('UI: external app authorization', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('Accept を押すと session が承認され userkey が発行される', async ({
    page,
    request,
    baseURL,
  }) => {
    const { app, session } = await startAuthSession(request);

    // 承認前は userkey を取れない。**この確認を先に置くのが要点**で、後段で
    // 取得できたときに「元から承認済みだった」可能性を排除できる。
    //
    // `auth/session/show` の `app.isAuthorized` は使わない。**認証済みリクエスト
    // でしか返らない** (`AppEntityService` が `me` の有無で出し分ける) ため、
    // 未認証の `callApi` では `undefined` になる。
    const beforeUserkey = await callApi(request, 'auth/session/userkey', {
      appSecret: app.secret,
      token: session.token,
    });
    expect(beforeUserkey.status()).not.toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/auth/${session.token}`, { waitUntil: 'domcontentloaded' });

    // 承認フォームが出るまで待つ。要求 permission も画面に出ているはず。
    const acceptButton = page.getByRole('button', { name: 'Accept' });
    await expect(acceptButton).toBeVisible({ timeout: 20_000 });

    await acceptButton.click();

    // 承認後は userkey が取れる。**API で直接 accept するのではなく画面操作の
    // 結果として発行される**ことがこの spec の主眼。
    await expect(async () => {
      const userkeyResp = await callApi(request, 'auth/session/userkey', {
        appSecret: app.secret,
        token: session.token,
      });
      expect(userkeyResp.status()).toBe(200);
      const userkey = (await userkeyResp.json()) as { accessToken: string };
      expect(typeof userkey.accessToken).toBe('string');
      expect(userkey.accessToken.length).toBeGreaterThan(0);
    }).toPass({ timeout: 15_000 });
  });

  test('Cancel を押すと承認されない', async ({ page, request, baseURL }) => {
    const { app, session } = await startAuthSession(request);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/auth/${session.token}`, { waitUntil: 'domcontentloaded' });

    const cancelButton = page.getByRole('button', { name: 'Cancel' });
    await expect(cancelButton).toBeVisible({ timeout: 20_000 });
    await cancelButton.click();

    // 拒否後も userkey は発行されない。**ここが通ってしまうと「押した内容に
    // 関わらず承認される」ことになり、承認画面が意味を失う。**
    const userkeyResp = await callApi(request, 'auth/session/userkey', {
      appSecret: app.secret,
      token: session.token,
    });
    expect(userkeyResp.status()).not.toBe(200);
  });

  test('要求された permission が画面に表示される', async ({ page, request, baseURL }) => {
    // 利用者が「何を許可するのか」を判断する材料。出ていなければ承認画面の
    // 意味が無いので、表示そのものを検証する。
    const { session } = await startAuthSession(request, ['read:account', 'write:notes']);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/auth/${session.token}`, { waitUntil: 'domcontentloaded' });

    await expect(page.getByRole('button', { name: 'Accept' })).toBeVisible({ timeout: 20_000 });

    // permission は `_permissions` の i18n 文言で `<ul><li>` に列挙される。
    // locale は en-US 固定なので実文言で照合できる。**「li が 1 つ以上ある」
    // だけだと他の list を拾いうるので、要求した permission の文言そのものを見る。**
    await expect(page.getByText('View your account information')).toBeVisible({
      timeout: 10_000,
    });
    await expect(page.getByText('Compose or delete notes')).toBeVisible({
      timeout: 10_000,
    });
  });

  test('未ログインだとログインを促される', async ({ page, request, baseURL }) => {
    const { session } = await startAuthSession(request);

    // signin せずに直接開く。auth.vue は `$i` が無いとログイン誘導を出す。
    await page.goto(`${baseURL}/auth/${session.token}`, { waitUntil: 'domcontentloaded' });

    // 承認ボタンは出てはいけない。**未ログインで承認できてしまうと、
    // token を知る第三者が任意のアカウントを連携させられる。**
    await expect(page.getByRole('button', { name: 'Accept' })).toHaveCount(0);
  });
});
