/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #820: block CRUD round-trip + 副作用 (= blocked user の follow
// reject)。
//
// upstream Misskey TS と mk-go は両方とも:
//   - blocking/create { userId } で block 関係を作成 (返却 status は backend 間
//     で drift あり、本 spec は 2xx range で許容)
//   - blocking/list で blockee 一覧を取得 ({ id, blockeeId, blockee? } 配列)
//   - blockee からの following/create は reject される (= 4xx、具体 status は
//     backend 間で異なるが error code は両者 BLOCKED)
//   - blocking/delete で関係解除 (= 同じく 2xx range)
//   - 解除後は再 follow 可能
//
// drop-in shape drift (= 後続 issue で揃える方向):
//   - blocking/create / blocking/delete の return shape (#870):
//     TS=200 + UserDetailed / mk-go=204 No Content
//   - blocked → following/create の reject status (#872):
//     TS=400 / mk-go=403
//   本 spec は 2xx / 4xx range で抽象化し、両 backend で同じ機能性
//   (= block + follow reject) を提供することにフォーカスする。
//
// 本 spec は両 backend 共通で:
//   1. A / B signup
//   2. A が B を block → blocking/list に B が出る
//   3. B が A を follow しようとして 4xx で reject されること
//   4. A が B を unblock → blocking/list から B が消え、B は A を follow
//      できる

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface BlockEntry {
  id: string;
  blockeeId: string;
}

test.describe('users: block CRUD + follow reject side-effect', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('block prevents follow; unblock allows follow again', async ({
    request,
  }) => {
    const blocker = await signupUser(request, randomUsername('bkA'));
    const blockee = await signupUser(request, randomUsername('bkB'));

    // create block → 2xx (status の drift を吸収)。
    const createResp = await callApi(request, 'blocking/create', {
      i: blocker.token,
      userId: blockee.id,
    });
    expect(createResp.status()).toBeGreaterThanOrEqual(200);
    expect(createResp.status()).toBeLessThan(300);

    // list で blockee が出ること。
    const listAfterCreate = await callApi(request, 'blocking/list', {
      i: blocker.token,
      limit: 100,
    });
    expect(listAfterCreate.status()).toBe(200);
    const listed = (await listAfterCreate.json()) as BlockEntry[];
    expect(listed.some((e) => e.blockeeId === blockee.id)).toBe(true);

    // blockee 視点の follow は reject される (= 4xx)。
    // status は backend 間で drift あり (TS=400 / mk-go=403)。本 spec は
    // 「reject されること」のみを保証し、具体的 status は後続 issue で
    // 揃える方向。
    const blockedFollow = await callApi(request, 'following/create', {
      i: blockee.token,
      userId: blocker.id,
    });
    expect(blockedFollow.status()).toBeGreaterThanOrEqual(400);
    expect(blockedFollow.status()).toBeLessThan(500);

    // delete block → 2xx (status の drift を吸収)。
    const deleteResp = await callApi(request, 'blocking/delete', {
      i: blocker.token,
      userId: blockee.id,
    });
    expect(deleteResp.status()).toBeGreaterThanOrEqual(200);
    expect(deleteResp.status()).toBeLessThan(300);

    // list から消えていること。
    const listAfterDelete = await callApi(request, 'blocking/list', {
      i: blocker.token,
      limit: 100,
    });
    expect(listAfterDelete.status()).toBe(200);
    const listed2 = (await listAfterDelete.json()) as BlockEntry[];
    expect(listed2.some((e) => e.blockeeId === blockee.id)).toBe(false);

    // 解除後は blockee → blocker の follow が成功する。
    const allowedFollow = await callApi(request, 'following/create', {
      i: blockee.token,
      userId: blocker.id,
    });
    expect(allowedFollow.status()).toBe(200);
  });
});
