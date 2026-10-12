/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定まわりの未カバーページと、通報通知先の管理画面を開く (#2441)。
//
//   /settings/connect                          サービス連携 (トークン発行の入口)
//   /admin/abuse-report-notification-recipient 通報の通知先
//
// `/settings/profiles` はここに含めない。保存済みプロファイルが 1 つも無いと
// **何も描画しない** (見出しすら出ない) ため、assert できるのは左メニューの
// 文言だけになり、`/settings/*` のどの URL でも通る偽陽性になる。プロファイルは
// クライアント側に保存されるもので API から用意できないので、別途 UI から
// 保存する手順を組んでから扱う。
//
// `/settings/connect` は `miauth/gen-token` を叩いてアクセストークンを発行する
// 入口。`/settings/apps` (発行済みの失効) と対になる画面で、そちらは
// `settings_apps_revoke.spec.ts` が見ている。
//
// 通報の通知先が壊れると **通報が届かなくなる**。モデレーターは気付きようが
// ないので、画面が出ることの確認だけでも価値がある。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: 設定・管理の未カバーページ', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('/settings/connect がトークン発行の入口を出す', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/settings/connect`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await expect(page.getByRole('button', { name: 'Generate access token' })).toBeVisible({
      timeout: 20_000,
    });
    // 発行済みトークンの管理へ渡す導線。ここが切れると失効できない。
    await expect(page.getByText('Manage access tokens', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('通報の通知先が一覧に表示され、追加ボタンが出る', async ({ page, baseURL, request }) => {
    // 事前に 1 件登録しておく。一覧が空だと「描画されている」のか
    // 「取得に失敗して空」なのか区別できない。
    //
    // method: 'email' は **宛先ユーザーにメールアドレスが要る** (未設定だと
    // CORRELATION_CHECK_EMAIL で 400)。テスト用の root には無いので webhook を使う。
    const stamp = Date.now().toString().slice(-9);
    const hook = await callApi(request, 'admin/system-webhook/create', {
      i: root.token,
      isActive: true,
      name: `pw-hook-${stamp}`,
      on: ['abuseReport'],
      url: 'https://example.invalid/pw-hook',
      secret: 'pw-secret',
    });
    expect(hook.status()).toBe(200);
    const webhook = (await hook.json()) as { id: string };

    const name = `pw-recipient-${stamp}`;
    const created = await callApi(
      request,
      'admin/abuse-report/notification-recipient/create',
      {
        i: root.token,
        isActive: true,
        name,
        method: 'webhook',
        systemWebhookId: webhook.id,
      },
    );
    expect(created.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/abuse-report-notification-recipient`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    await expect(page.getByText('Add recipient for reports', { exact: false }).first()).toBeVisible(
      { timeout: 20_000 },
    );
    // 通知先が出ないと、通報が誰にも届かない状態に気付けない。
    await expect(page.getByText(name, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
  });
});
