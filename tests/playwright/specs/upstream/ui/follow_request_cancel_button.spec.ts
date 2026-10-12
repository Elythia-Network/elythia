/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/follow-requests の sent tab で Cancel button click → confirm OK →
// /api/following/requests/cancel round-trip する write-flow spec。
//
// follow-requests.vue:25 の cancel button は ti-x icon + "Cancel" text +
// danger style。click すると os.confirm question → 承諾後
// following/requests/cancel を叩く (line 87)。
// setup:
//   1. root を unlocked にする (sent tab を default 表示するため)
//   2. signupUser で locked target を作る (i/update isLocked=true)
//   3. root → target を follow → locked なので request 化
//   4. /my/follow-requests (default sent tab) → Cancel click → confirm OK
// で round-trip を verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /my/follow-requests cancel button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('signup locked target + send follow request → Cancel click → /api/following/requests/cancel', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. root を unlocked にする (default tab が "sent" になる)
    await callApi(request, 'i/update', { i: root.token, isLocked: false });

    // 2. target user を signup + 鍵設定
    const target = await signupUser(request, `pwfrc${Date.now().toString().slice(-9)}`);
    await callApi(request, 'i/update', { i: target.token, isLocked: true });

    // 3. root → target を follow (target が locked なので request 化)
    const followResp = await callApi(request, 'following/create', {
      i: root.token,
      userId: target.id,
    });
    expect(followResp.status()).toBe(200);

    // 4. /my/follow-requests を root として開く (sent tab default)
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/my/follow-requests`, {
      waitUntil: 'domcontentloaded',
    });

    // target username が body に出るまで待つ (= sent list 反映)
    await page.waitForFunction(
      (u) => document.body.textContent?.includes(u) ?? false,
      target.username,
      { timeout: 20_000 },
    );

    // 5. Cancel button (= "Cancel" text + ti-x icon) hydrate を待つ
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some(
          (b) =>
            (b.textContent ?? '').includes('Cancel') &&
            b.querySelector('i.ti-x') !== null,
        );
      },
      { timeout: 15_000 },
    );

    // Cancel click → confirm dialog 出現
    await clickWhenReady(page, '「Cancel」のボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          (b.textContent ?? '').includes('Cancel') &&
          b.querySelector('i.ti-x') !== null,
      ),
    );

    // 6. confirm dialog OK click → following/requests/cancel
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    const cancelResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/following/requests/cancel') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await cancelResp;

    // 7. API 経由で 関係性 verify: pending request が消える
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
    expect(rel.hasPendingFollowRequestFromYou).toBe(false);
  });
});
