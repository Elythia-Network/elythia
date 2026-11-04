/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// サインインの helper (fixtures/ui_auth.ts) が、本当にその利用者として
// サインインした状態を作るかを確かめる (#4026)。
//
// uiSigninAsRoot はフォームを通さず、globalSetup が保存した state を読み込む。
// uiSigninAs は spec ごとに登録した利用者でフォームを通す。どちらも、ページが
// 持っている token でサーバーに問い合わせ、返ってきた利用者を見る。helper の
// 中の確認 (localStorage の `account`) とは別の経路で見るのは、その確認自体が
// 壊れても気付けるようにするため。

import { readFileSync } from 'node:fs';
import { expect, type Page, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAs, uiSigninAsRoot } from '../../../fixtures/ui_auth';

// ページの localStorage にある token で `i` を呼び、サーバーから見た利用者名を返す
async function serverSideUsername(page: Page): Promise<string | null> {
  return page.evaluate(async () => {
    const raw = window.localStorage.getItem('account');
    if (raw === null) return null;
    const { token } = JSON.parse(raw) as { token?: string };
    const resp = await fetch('/api/i', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ i: token }),
    });
    if (!resp.ok) return null;
    return ((await resp.json()) as { username: string }).username;
  });
}

test.describe('UI: signin helpers sign in as the intended user', () => {
  // 名前を変えた test の後に、元の名前へ戻す。test の中の finally では戻さない。
  // test が時間切れになると、Playwright は本体を待たずに fixture を片付けるので、
  // finally の callApi は片付けた後の request で失敗し、root の名前が残る。
  // afterEach は fixture が生きているうちに走る
  let rootNameToRestore: { token: string; name: string | null } | null = null;
  test.afterEach(async ({ request }) => {
    if (rootNameToRestore === null) return;
    const { token, name } = rootNameToRestore;
    rootNameToRestore = null;
    const restored = await callApi(request, 'i/update', { i: token, name });
    expect(restored.ok(), 'failed to restore root name').toBe(true);
  });

  test('uiSigninAsRoot signs in as root without the form', async ({ page, baseURL }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    const flows: string[] = [];
    page.on('request', (req) => {
      if (req.url().includes('/api/signin-flow')) flows.push(req.url());
    });
    await uiSigninAsRoot(page, baseURL!, root);
    expect(await serverSideUsername(page)).toBe(root.username);
    // フォームに戻っていたら、注入が効かずに遅い経路を通っている
    expect(flows, 'uiSigninAsRoot went through the signin form').toEqual([]);
  });

  // 保存した state の `account` は globalSetup の時点の値。そのまま読み込むと、
  // 設定の画面が古い値で mount する (uiSigninAsRoot が今の値に差し替える)
  test('uiSigninAsRoot loads the current account data, not the saved one', async ({ page, request, baseURL }) => {
    const root: RootFixture = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
    const before = await callApi(request, 'i', { i: root.token });
    expect(before.ok()).toBe(true);
    rootNameToRestore = { token: root.token, name: ((await before.json()) as { name: string | null }).name };
    const name = randomUsername('fresh');
    const upd = await callApi(request, 'i/update', { i: root.token, name });
    expect(upd.ok()).toBe(true);
    // frontend は起動の後で `account` を取り直すので、後から読むと差し替えが
    // 無くても新しい値になりうる。アプリのスクリプトより前に、起動の時点の値を
    // 記録しておいて、それを見る
    await page.addInitScript(() => {
      const raw = window.localStorage.getItem('account');
      (window as unknown as { bootAccountName?: string | null }).bootAccountName =
        raw === null ? null : (JSON.parse(raw) as { name: string | null }).name;
    });
    await uiSigninAsRoot(page, baseURL!, root);
    const atBoot = await page.evaluate(() => (window as unknown as { bootAccountName?: string | null }).bootAccountName);
    expect(atBoot).toBe(name);
  });

  test('uiSigninAs signs in as the given user, not root', async ({ page, request, baseURL }) => {
    const user = await signupUser(request, randomUsername('pwiso'));
    await uiSigninAs(page, request, baseURL!, user);
    expect(await serverSideUsername(page)).toBe(user.username);
  });
});
