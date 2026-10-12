/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #836: following/requests/* round-trip (= follow request flow)。
//
// upstream Misskey TS と mk-go は両方とも:
//   - i/update { isLocked: true } で account を locked にすると、follow は
//     request を経由する
//   - A → B (locked) に following/create で follow request を送る
//   - B 視点 following/requests/list に A が含まれる (= 受信中)
//   - A 視点 following/requests/sent に B が含まれる (= 送信中)
//   - reject path: B が following/requests/reject で拒否 → A の sent から消える
//   - accept path: 別 user pair で C が D の request を accept → 両者 follow 関係成立
//   - cancel path: 別 user pair で sender が自分で cancel → request 消える
//
// 本 spec は 3 path (reject / accept / cancel) を 1 test で順次検証する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface FollowRequestEntry {
  id: string;
  follower?: { id: string };
  followee?: { id: string };
}

async function lockAccount(
  request: import('@playwright/test').APIRequestContext,
  token: string,
): Promise<void> {
  const resp = await callApi(request, 'i/update', { i: token, isLocked: true });
  expect(resp.status()).toBe(200);
}

test.describe('following/requests round-trip', () => {
  // 3 path (reject / accept / cancel) を別 test で回すので各 test で 2
  // user signup する。signup は IP base rate limit (5 / 1h) があるので
  // beforeEach で毎回 reset しないと 3 test 目で 429 になる。
  test.beforeEach(() => {
    resetRateLimit();
  });

  test('reject path: A → B(locked) request → list shows / B rejects → sent cleared', async ({
    request,
  }) => {
    const A = await signupUser(request, randomUsername('frRA'));
    const B = await signupUser(request, randomUsername('frRB'));
    await lockAccount(request, B.token);

    // A が B に follow request を送る
    const followResp = await callApi(request, 'following/create', {
      i: A.token,
      userId: B.id,
    });
    expect([200, 204]).toContain(followResp.status());

    // B の受信 list に A が含まれる
    const listResp = await callApi(request, 'following/requests/list', { i: B.token });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as FollowRequestEntry[];
    expect(list.find((r) => r.follower?.id === A.id)).toBeDefined();

    // A の送信 list (sent) に B が含まれる
    const sentResp = await callApi(request, 'following/requests/sent', { i: A.token });
    expect(sentResp.status()).toBe(200);
    const sent = (await sentResp.json()) as FollowRequestEntry[];
    expect(sent.find((r) => r.followee?.id === B.id)).toBeDefined();

    // B が reject
    const rejectResp = await callApi(request, 'following/requests/reject', {
      i: B.token,
      userId: A.id,
    });
    expect([200, 204]).toContain(rejectResp.status());

    // reject 後 A の sent から B が消える
    const sentAfter = await callApi(request, 'following/requests/sent', { i: A.token });
    expect(sentAfter.status()).toBe(200);
    const sentAfterBody = (await sentAfter.json()) as FollowRequestEntry[];
    expect(sentAfterBody.find((r) => r.followee?.id === B.id)).toBeFalsy();

    // 受信側 (B) の list からも A が消える (= 両方向 cleanup を担保)
    const listAfterReject = await callApi(request, 'following/requests/list', { i: B.token });
    expect(listAfterReject.status()).toBe(200);
    const listAfterRejectBody = (await listAfterReject.json()) as FollowRequestEntry[];
    expect(listAfterRejectBody.find((r) => r.follower?.id === A.id)).toBeFalsy();
  });

  test('accept path: C → D(locked) request → D accepts → users/following reflects', async ({
    request,
  }) => {
    const C = await signupUser(request, randomUsername('frAC'));
    const D = await signupUser(request, randomUsername('frAD'));
    await lockAccount(request, D.token);

    // C が D に follow request を送る
    const followResp = await callApi(request, 'following/create', {
      i: C.token,
      userId: D.id,
    });
    expect([200, 204]).toContain(followResp.status());

    // D が accept
    const acceptResp = await callApi(request, 'following/requests/accept', {
      i: D.token,
      userId: C.id,
    });
    expect([200, 204]).toContain(acceptResp.status());

    // accept 後 C の users/following に D が含まれる (= follow 確定)
    const followingResp = await callApi(request, 'users/following', {
      i: C.token,
      userId: C.id,
    });
    expect(followingResp.status()).toBe(200);
    const followingList = (await followingResp.json()) as Array<{ followeeId: string }>;
    expect(followingList.find((f) => f.followeeId === D.id)).toBeDefined();

    // 反対側 (D の followers) でも C が含まれる = 両方向に follow 関係が
    // 整合的に成立していることを担保する
    const followersResp = await callApi(request, 'users/followers', {
      i: D.token,
      userId: D.id,
    });
    expect(followersResp.status()).toBe(200);
    const followersList = (await followersResp.json()) as Array<{ followerId: string }>;
    expect(followersList.find((f) => f.followerId === C.id)).toBeDefined();
  });

  test('cancel path: E → F(locked) request → E cancels → list cleared', async ({
    request,
  }) => {
    const E = await signupUser(request, randomUsername('frCE'));
    const F = await signupUser(request, randomUsername('frCF'));
    await lockAccount(request, F.token);

    // E が F に request
    const followResp = await callApi(request, 'following/create', {
      i: E.token,
      userId: F.id,
    });
    expect([200, 204]).toContain(followResp.status());

    // E が cancel
    const cancelResp = await callApi(request, 'following/requests/cancel', {
      i: E.token,
      userId: F.id,
    });
    expect([200, 204]).toContain(cancelResp.status());

    // cancel 後 F の受信 list から E が消える
    const listAfter = await callApi(request, 'following/requests/list', { i: F.token });
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as FollowRequestEntry[];
    expect(listAfterBody.find((r) => r.follower?.id === E.id)).toBeFalsy();
  });
});
