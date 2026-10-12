/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/relays で Remove button (ti-trash + "Remove") click →
// /api/admin/relays/remove round-trip する write-flow spec。
//
// admin/relays.vue:19 の MkButton danger は ti-trash + "Remove" text。
// click すると confirm dialog 無しで misskeyApi('admin/relays/remove') を
// 直接叩く (line 57-58)。admin_relay_add_form spec の inverse として、
// remove path を strict 化する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/relays remove button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('Remove button → /api/admin/relays/remove (no confirm dialog)', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 relay を API で add (URL は spec 専用 hostname で重複回避)
    const inbox = `https://pwrelay-${Date.now().toString().slice(-9)}.invalid/inbox`;
    const addResp = await callApi(request, 'admin/relays/add', {
      i: root.token,
      inbox,
    });
    expect(addResp.status()).toBe(200);

    // 2. /admin/relays を開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/relays`, {
      waitUntil: 'domcontentloaded',
    });

    // inbox URL が body に出るまで待つ (= list 反映)
    await page.waitForFunction(
      (i) => document.body.textContent?.includes(i) ?? false,
      inbox,
      { timeout: 20_000 },
    );

    // 3. 追加した relay 自身の Remove button (= ti-trash) を待つ。
    //
    // **panel を「inbox を含む最初の div」で探してはいけない。** div は入れ子
    // なので、その条件では relay 一覧をまとめて包む**外側の div** が先に
    // 当たり、そこから最初の trash button を取ると**別の relay** を消す。
    // それでも `admin/relays/remove` は 2xx を返すので response 待ちは通り、
    // 最後の一覧確認だけが落ちる。
    //
    // 実際 admin_relay_add_form spec が relay を残すため一覧は常に複数件で、
    // mk-go では対象がたまたま先頭に来て通っていた。TS backend で一覧の順序が
    // 違い、初めて表面化した (#2289)。
    //
    // `.last()` が要点。入れ子の div は外側が先に列挙されるので、最後に来るのが
    // 「inbox と trash button の両方を含む最も内側の div」= その relay の panel。
    const panel = page
      .locator('div')
      .filter({ hasText: inbox })
      .filter({ has: page.locator('button i.ti-trash') })
      .last();
    const removeButton = panel.locator('button:has(i.ti-trash)').last();
    await expect(removeButton).toBeVisible({ timeout: 15_000 });

    // 4. 該当 relay の Remove button click → admin/relays/remove
    const removeResp = page.waitForResponse(
      (r) => r.url().includes('/api/admin/relays/remove') && r.status() < 300,
      { timeout: 15_000 },
    );
    await removeButton.click();
    await removeResp;

    // 5. API 経由で削除確認
    const listResp = await callApi(request, 'admin/relays/list', {
      i: root.token,
    });
    expect(listResp.status()).toBe(200);
    const list = await listResp.json();
    const found = list.find((r: { inbox: string }) => r.inbox === inbox);
    expect(found).toBeUndefined();
  });
});
