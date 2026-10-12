/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /@username の 3-dot menu → menu の "Unblock" item (ti-fw ti-ban) を
// click → confirm OK → /api/blocking/delete round-trip する write-flow spec。
//
// get-user-menu.ts:79 で `getConfirmed(...)` を unblock 経路でも経由する
// (`user.isBlocking ? unblockConfirm : blockConfirm` の三項のため両 case
// で confirm)。本 spec は confirm OK click を経て unblock を完了させる。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: user 3-dot menu unblock flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('signup target → root blocks via API → /@target menu → Unblock → /api/blocking/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    const target = await signupUser(request, `pwub${Date.now().toString().slice(-9)}`);
    expect(target.id).toBeTruthy();

    // root が target を block (= isBlocking=true)
    const blockResp = await callApi(request, 'blocking/create', {
      i: root.token,
      userId: target.id,
    });
    expect(blockResp.status()).toBeLessThan(400);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/@${target.username}`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      (u) => document.body.textContent?.includes(u) ?? false,
      target.username,
      { timeout: 20_000 },
    );

    // 3-dot menu click
    await clickButtonWithIcon(page, 'i.ti-dots');

    // "Unblock" item (= ti-fw ti-ban) を待って click。block / unblock どちら
    // でも icon は ti-ban (= text 切替のみ) なので、isBlocking=true 状態の
    // menu item は "Unblock" 動作を行う。
    await clickButtonWithIcon(page, 'i.ti-fw.ti-ban');

    // confirm dialog の OK click → /api/blocking/delete
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );
    const unblockResp = page.waitForResponse(
      (r) => r.url().includes('/api/blocking/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await unblockResp;

    // API verify: relation isBlocking=false
    const relResp = await callApi(request, 'users/relation', {
      i: root.token,
      userId: target.id,
    });
    expect(relResp.status()).toBe(200);
    // upstream users/relation は単一 userId (string) でも
    // `.then(it => [it])` で **必ず配列** を返す (relation.ts:135-137)。
    // mk-go も #1766 で配列に揃えているので、単体オブジェクトとして
    // 扱うと全 field が undefined になる。
    const [rel] = await relResp.json();
    expect(rel.isBlocking).toBe(false);
  });
});
