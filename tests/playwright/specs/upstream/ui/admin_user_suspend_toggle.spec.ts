/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/user?userId=:id で suspend MkSwitch を click → confirm dialog の
// OK → /api/admin/suspend-user が走る write-flow spec。
//
// admin-user.vue の toggleSuspend は MkSwitch v-model 変化で起動する
// (line 97 + 347-358)。click → os.confirm() で warning dialog → 承諾後に
// admin/suspend-user 叩いて refresh。本 spec は新規 user を signup して
// 即 suspend を toggle、API 経由で isSuspended が true になっていることを
// 確認する。root を巻き添え suspend しないよう必ず別 user を target にする。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /admin/user suspend toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('toggle suspend switch + confirm OK → /api/admin/suspend-user', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. target user を signup (root を suspend しないように必ず別 user)
    const username = `pwsus${Date.now().toString().slice(-9)}`;
    const target = await signupUser(request, username);
    expect(target.id).toBeTruthy();

    // 2. /admin/user/:userId を root として開く。
    // upstream の route definition (router.definition.ts:381) は
    // **path-based** `/admin/user/:userId` で query string `?userId=` ではない。
    // 旧実装は query 渡しで not-found に近い page (= 404 fallback) に navigate
    // し、username が body に出ず 20s timeout していた。
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(
      `${baseURL}/admin/user/${target.id}`,
      { waitUntil: 'domcontentloaded' },
    );
    expect(resp!.status()).toBe(200);

    // user info hydrate を待つ (username が body に出る)
    await page.waitForFunction(
      (u) => document.body.textContent?.includes(u) ?? false,
      target.username,
      { timeout: 20_000 },
    );

    // 3. suspend switch (= "Suspend" label を含む checkbox) を click
    // /admin/user は admin moderation switches が複数並ぶ。"Suspend" は
    // MkSwitch label に i18n.ts.suspend (= "Suspend") を表示するため、
    // text 周辺の checkbox を取る。簡易には全 checkbox を取って index で
    // 当てる: admin-user.vue 順は moderator / silence / suspend / 他なので
    // 4 つ目以降。signup 直後 user では事前 toggle 無しで suspend は
    // false → 押すと true になる。
    //
    // 確実性のため、label 文字列で switch を識別する: "Suspend" を含む
    // 親要素を探し、その配下の checkbox を click する。
    await page.waitForFunction(
      () => {
        const labels = Array.from(document.querySelectorAll('label, span'));
        return labels.some((l) =>
          (l.textContent ?? '').trim().match(/^Suspend$/i),
        );
      },
      { timeout: 15_000 },
    );

    // confirm dialog の OK button (data-cy-modal-dialog-ok) が後で出てくる。
    // suspend click → dialog open → OK click → API call。response 待ちは
    // OK click 後に始める。
    await page.evaluate(() => {
      // Suspend label を持つ要素から最も近い checkbox を探す
      const labels = Array.from(document.querySelectorAll('label, span, div'));
      const suspendLabel = labels.find(
        (l) => (l.textContent ?? '').trim().match(/^Suspend$/i),
      );
      if (!suspendLabel) return;
      // 親方向に MkSwitch の root を辿って配下の checkbox を取る
      let node: HTMLElement | null = suspendLabel as HTMLElement;
      for (let depth = 0; depth < 6 && node; depth++) {
        const cb = node.querySelector('input[type="checkbox"]') as HTMLInputElement | null;
        if (cb) {
          cb.click();
          return;
        }
        node = node.parentElement;
      }
    });

    // 4. confirm dialog OK click → admin/suspend-user 走る
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );
    const suspendResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/admin/suspend-user') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await suspendResp;

    // 5. API 経由で target.isSuspended=true を verify
    const showResp = await callApi(request, 'admin/show-user', {
      i: root.token,
      userId: target.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    // upstream の admin/show-user は id を返さない (show-user.ts の return は
    // profile 由来の field と roles/policies/signins のみ)。mk-go は追加で id を
    // 返すが、両 backend で通る assert にするため id は見ない (#2276)。
    expect(shown.isSuspended).toBe(true);
  });
});
