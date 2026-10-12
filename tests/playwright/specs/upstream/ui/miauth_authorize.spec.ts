/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// MiAuth の承認画面 (/miauth/:session) を実ブラウザで操作する (#2441)。
//
// API レイヤは `specs/upstream/api/miauth/miauth.spec.ts` が検証済みだが、そちらは
// `miauth/gen-token` を **API で直接叩いており**、承認画面を人が操作する経路は
// 未検証だった。
//
// `/auth/:token` (auth_authorize.spec.ts) とは **UI が別物**なので spec を分ける。
//
//   - `/auth` は `auth.form.vue` — Accept / Cancel の 2 ボタンだけ
//   - `/miauth` は `MkAuthConfirm.vue` — **アカウント選択 → 承認の 2 段階**で、
//     ボタン文言も Accept / Reject
//
// 同じ「認可画面」でもコンポーネントが違うため、片方が通っても他方は担保されない。
//
// ボタンは `data-testid` を持たないのでテキストで特定する。playwright.config.ts が
// browser locale を en-US に固定しているため安定する (vite の hash class は
// production ビルドで変わるので使わない)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

/** MiAuth の session id。UUID である必要は無く、アプリ側が生成する任意の文字列。 */
function newSession(): string {
  return `pw-miauth-${Date.now()}-${Math.floor(Math.random() * 100000)}`;
}

/**
 * 承認画面を開き、アカウント選択段階を通過して承認段階まで進める。
 *
 * `MkAuthConfirm` は `accountSelect` phase を先に出す。既にログイン済みでも
 * 「どのアカウントで許可するか」を尋ねる作りで、**アカウントを選ぶまで Continue は
 * `disabled`**。radio を選ばずに Continue を押そうとしても進めず、Accept が
 * 見つからないまま timeout する。
 */
async function reachConsentPhase(page: import('@playwright/test').Page): Promise<void> {
  const continueButton = page.getByRole('button', { name: 'Continue' });
  const acceptButton = page.getByRole('button', { name: 'Accept' });

  // **まず SPA の hydration を待つ。** `page.goto` は `domcontentloaded` で返るので、
  // 直後は両方とも DOM に無い。ここで `isVisible()` を見ると必ず false になり、
  // 「accountSelect phase が無い」と誤判定して素通りする (実際それで 3 回落とした)。
  await expect(continueButton.or(acceptButton).first()).toBeVisible({ timeout: 20_000 });

  if (!(await continueButton.isVisible().catch(() => false))) {
    return; // 既に consent phase (accountSelect を出さない構成)
  }

  // radio 本体は CSS で隠れており `<label for>` がクリック対象。
  // `check({force:true})` は DOM の checked を立てるだけで **Vue の v-model が
  // 反応せず Continue が disabled のまま**なので、label を押して本物の input
  // イベントを起こす。
  await page.locator('label[for^="account-"]').first().click();
  await expect(continueButton).toBeEnabled({ timeout: 10_000 });
  await continueButton.click();
}

test.describe('UI: MiAuth authorization', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('Accept を押すと token が発行され API に使える', async ({ page, request, baseURL }) => {
    const session = newSession();

    // 承認前に token は取れない。**これを先に置くことで、後段の成功が
    // 「元から発行済みだった」ものでないと言える。**
    const before = await callApi(request, `miauth/${session}/check`);
    expect(before.status()).toBe(200);
    expect((await before.json()).ok).toBe(false);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(
      `${baseURL}/miauth/${session}?name=pw-miauth-app&permission=read:account`,
      { waitUntil: 'domcontentloaded' },
    );

    await reachConsentPhase(page);

    const acceptButton = page.getByRole('button', { name: 'Accept' });
    await expect(acceptButton).toBeVisible({ timeout: 20_000 });
    await acceptButton.click();

    // 承認後は check が token を返す。**API で gen-token を叩くのではなく
    // 画面操作の結果として発行される**ことがこの spec の主眼。
    await expect(async () => {
      const check = await callApi(request, `miauth/${session}/check`);
      expect(check.status()).toBe(200);
      const body = (await check.json()) as { ok: boolean; token?: string };
      expect(body.ok).toBe(true);
      expect(typeof body.token).toBe('string');
      expect(body.token!.length).toBeGreaterThan(0);
    }).toPass({ timeout: 15_000 });
  });

  test('Reject を押すと token が発行されない', async ({ page, request, baseURL }) => {
    const session = newSession();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(
      `${baseURL}/miauth/${session}?name=pw-miauth-app&permission=read:account`,
      { waitUntil: 'domcontentloaded' },
    );

    await reachConsentPhase(page);

    const rejectButton = page.getByRole('button', { name: 'Reject' });
    await expect(rejectButton).toBeVisible({ timeout: 20_000 });
    await rejectButton.click();

    // 拒否しても token が出るなら、承認画面が意味を失う。
    const check = await callApi(request, `miauth/${session}/check`);
    expect(check.status()).toBe(200);
    expect((await check.json()).ok).toBe(false);
  });

  test('要求された permission が画面に表示される', async ({ page, baseURL }) => {
    const session = newSession();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(
      `${baseURL}/miauth/${session}?name=pw-miauth-app&permission=read:account,write:notes`,
      { waitUntil: 'domcontentloaded' },
    );

    await reachConsentPhase(page);
    await expect(page.getByRole('button', { name: 'Accept' })).toBeVisible({ timeout: 20_000 });

    // 利用者が「何を許可するのか」を判断する材料。`_permissions` の i18n 文言で
    // 列挙されるので、実文言で照合する (locale は en-US 固定)。
    await expect(page.getByText('View your account information')).toBeVisible({
      timeout: 10_000,
    });
    await expect(page.getByText('Compose or delete notes')).toBeVisible({
      timeout: 10_000,
    });
  });

  test('未ログインだと承認できない', async ({ page, baseURL }) => {
    const session = newSession();

    // signin せずに直接開く。**未ログインで承認できてしまうと、session id を
    // 知る第三者が任意のアカウントの token を得られる。**
    await page.goto(
      `${baseURL}/miauth/${session}?name=pw-miauth-app&permission=read:account`,
      { waitUntil: 'domcontentloaded' },
    );

    await expect(page.getByRole('button', { name: 'Accept' })).toHaveCount(0);
  });
});
