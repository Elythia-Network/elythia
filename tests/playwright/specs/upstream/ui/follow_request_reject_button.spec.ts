/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/follow-requests で list tab → Reject button click →
// /api/following/requests/reject round-trip する write-flow spec。
//
// follow-requests.vue:22 の reject button は ti-x icon + "Reject" text +
// danger style。本 spec は accept_button spec と setup 同じ:
//   1. root を isLocked=true に設定
//   2. signupUser で requester を作る
//   3. requester → root の follow を作る
//   4. uiSigninAsRoot → /my/follow-requests
//   5. Reject click → /api/following/requests/reject
//   6. cleanup
// で round-trip を verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /my/follow-requests reject button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('lock root + signup requester + follow → Reject click → /api/following/requests/reject', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. root を locked にする
    await callApi(request, 'i/update', { i: root.token, isLocked: true });

    // 2. requester を signup
    const requester = await signupUser(request, `pwfrr${Date.now().toString().slice(-9)}`);

    // 3. requester → root を follow (locked なので request 化)
    const followResp = await callApi(request, 'following/create', {
      i: requester.token,
      userId: root.id,
    });
    expect(followResp.status()).toBe(200);

    // 4. /my/follow-requests を root として開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/my/follow-requests`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      (u) => document.body.textContent?.includes(u) ?? false,
      requester.username,
      { timeout: 20_000 },
    );

    // 5. Reject button (= "Reject" text + ti-x icon) hydrate を待つ。
    // accept は ti-check + "Accept"、reject は ti-x + "Reject"。
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some(
          (b) =>
            (b.textContent ?? '').includes('Reject') &&
            b.querySelector('i.ti-x') !== null,
        );
      },
      { timeout: 15_000 },
    );

    // 6. Reject click → following/requests/reject round-trip
    const rejectResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/following/requests/reject') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '「Reject」のボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          (b.textContent ?? '').includes('Reject') &&
          b.querySelector('i.ti-x') !== null,
      ),
    );
    await rejectResp;

    // 7. API 経由で関係性 verify: requester は root を follow していない
    const relResp = await callApi(request, 'users/relation', {
      i: requester.token,
      userId: root.id,
    });
    expect(relResp.status()).toBe(200);
    // upstream users/relation は単一 userId (string) でも
    // `.then(it => [it])` で **必ず配列** を返す (relation.ts:135-137)。
    // mk-go も #1766 で配列に揃えているので、単体オブジェクトとして
    // 扱うと全 field が undefined になる。
    const [rel] = await relResp.json();
    expect(rel.isFollowing).toBe(false);
    expect(rel.hasPendingFollowRequestFromYou).toBe(false);

    // 8. cleanup: root の isLocked を false に戻す
    await callApi(request, 'i/update', { i: root.token, isLocked: false });
  });
});
