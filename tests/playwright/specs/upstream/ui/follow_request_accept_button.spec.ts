/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/follow-requests で list tab → Accept button click →
// /api/following/requests/accept round-trip する write-flow spec。
//
// follow-requests.vue:21 の accept button は ti-check icon + "Accept" text、
// list tab (= 自分宛の request) で表示される。本 spec は:
//   1. root を isLocked=true に設定
//   2. signupUser で requester を作る
//   3. requester → root の follow を作る (鍵だから request 化)
//   4. uiSigninAsRoot → /my/follow-requests に navigate
//   5. Accept click → /api/following/requests/accept
//   6. cleanup: root の isLocked を false に戻す
// で round-trip を verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /my/follow-requests accept button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('lock root + signup requester + follow → Accept click → /api/following/requests/accept', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. root を locked にする (= follow request が来るようにする)
    await callApi(request, 'i/update', { i: root.token, isLocked: true });

    // 2. requester を signup
    const requester = await signupUser(request, `pwfra${Date.now().toString().slice(-9)}`);

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

    // requester username が body に出るまで待つ (= list 反映)
    await page.waitForFunction(
      (u) => document.body.textContent?.includes(u) ?? false,
      requester.username,
      { timeout: 20_000 },
    );

    // 5. Accept button (= "Accept" text + ti-check icon) hydrate を待つ
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some(
          (b) =>
            (b.textContent ?? '').includes('Accept') &&
            b.querySelector('i.ti-check') !== null,
        );
      },
      { timeout: 15_000 },
    );

    // 6. Accept click → following/requests/accept round-trip
    const acceptResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/following/requests/accept') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '「Accept」のボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          (b.textContent ?? '').includes('Accept') &&
          b.querySelector('i.ti-check') !== null,
      ),
    );
    await acceptResp;

    // 7. API 経由で関係性 verify: requester is now following root.
    // #984 drift fix で users/relation が実 DB 状態を返すようになったので、
    // requester 視点で isFollowing=true を確認する strict path に戻す。
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
    expect(rel.isFollowing).toBe(true);

    // 8. cleanup: root の isLocked を false に戻す (他 spec への副作用回避)
    await callApi(request, 'i/update', { i: root.token, isLocked: false });
  });
});
