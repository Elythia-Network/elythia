/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #820: follow CRUD と users/following の round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - following/create { userId } で follow 関係を確立し、対象 user の
//     UserDetailed entity を返す
//   - users/following { userId } で follower 視点の following list を取得
//     (response は { id, followerId, followeeId, follower?, followee? } の
//     relation array)
//   - following/delete { userId } で関係を解除し、users/following list から
//     対象 user が消える
//
// 本 spec は両 backend 共通で:
//   1. follower (A) / followee (B) signup
//   2. A が B を follow → response の id check
//   3. A の users/following が B を含むことを確認
//   4. A が B を unfollow
//   5. A の users/following から B が消えていることを確認

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface FollowingEntry {
  id: string;
  followerId: string;
  followeeId: string;
}

test.describe('users: follow CRUD + users/following round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('A follows then unfollows B; users/following reflects both transitions', async ({
    request,
  }) => {
    const follower = await signupUser(request, randomUsername('fwA'));
    const followee = await signupUser(request, randomUsername('fwB'));

    // create follow。response は followee の UserDetailed (id 一致を確認)。
    const createResp = await callApi(request, 'following/create', {
      i: follower.token,
      userId: followee.id,
    });
    expect(createResp.status()).toBe(200);
    const created = await createResp.json();
    expect(created.id).toBe(followee.id);

    // users/following で follower の following list を取得し、followee を
    // 含むこと。
    const listAfterCreate = await callApi(request, 'users/following', {
      i: follower.token,
      userId: follower.id,
      limit: 100,
    });
    expect(listAfterCreate.status()).toBe(200);
    const listed = (await listAfterCreate.json()) as FollowingEntry[];
    expect(listed.some((e) => e.followeeId === followee.id)).toBe(true);

    // delete follow。response は同じく followee の UserDetailed。
    const deleteResp = await callApi(request, 'following/delete', {
      i: follower.token,
      userId: followee.id,
    });
    expect(deleteResp.status()).toBe(200);

    // users/following から followee が消えていること。
    const listAfterDelete = await callApi(request, 'users/following', {
      i: follower.token,
      userId: follower.id,
      limit: 100,
    });
    expect(listAfterDelete.status()).toBe(200);
    const listed2 = (await listAfterDelete.json()) as FollowingEntry[];
    expect(listed2.some((e) => e.followeeId === followee.id)).toBe(false);
  });
});
