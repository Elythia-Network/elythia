/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #833: renote-mute CRUD round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - renote-mute/create { userId } で mutee を登録 (204)
//   - renote-mute/list で自分の mutee 一覧 (= UserDetailed embed)
//   - renote-mute/delete { userId } で削除 (204)
//
// 通常の mute (= 元 note ごと非表示) と区別するための実 timeline 検証は
// 別 spec で扱う想定 (#903 で follow-up)。本 spec は CRUD round-trip +
// duplicate / self-mute の error path に集中する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RenoteMuteEntry {
  id: string;
  createdAt: string;
  muteeId: string;
}

test.describe('renote-mute: CRUD round-trip', () => {
  // assertion 失敗時に renote_muting row が orphan として残らないよう
  // afterEach で best-effort cleanup する。正規 path で delete 済の場合は
  // 4xx (NOT_MUTING) が返るが idempotent に許容する。
  let muterToken: string | undefined;
  let muteeId: string | undefined;

  test.beforeAll(() => {
    resetRateLimit();
  });

  test.afterEach(async ({ request }) => {
    if (muterToken && muteeId) {
      await callApi(request, 'renote-mute/delete', {
        i: muterToken,
        userId: muteeId,
      });
    }
    muterToken = undefined;
    muteeId = undefined;
  });

  test('create / list / delete round-trip + error paths', async ({ request }) => {
    const A = await signupUser(request, randomUsername('rmA'));
    const B = await signupUser(request, randomUsername('rmB'));
    muterToken = A.token;
    muteeId = B.id;

    // create
    const createResp = await callApi(request, 'renote-mute/create', {
      i: A.token,
      userId: B.id,
    });
    expect([200, 204]).toContain(createResp.status());

    // list で B が含まれる
    const listResp = await callApi(request, 'renote-mute/list', { i: A.token });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as RenoteMuteEntry[];
    expect(Array.isArray(list)).toBe(true);
    expect(list.find((m) => m.muteeId === B.id)).toBeTruthy();

    // 重複 create は 400 + ALREADY_MUTING (両 backend で同 error code)。
    // status 範囲ではなく code を直接 assert することで shape drift を guard。
    const dupResp = await callApi(request, 'renote-mute/create', {
      i: A.token,
      userId: B.id,
    });
    expect(dupResp.status()).toBe(400);
    const dupBody = (await dupResp.json()) as { error?: { code?: string } };
    expect(dupBody.error?.code).toBe('ALREADY_MUTING');

    // self-mute は 400 + MUTEE_IS_YOURSELF。
    const selfResp = await callApi(request, 'renote-mute/create', {
      i: A.token,
      userId: A.id,
    });
    expect(selfResp.status()).toBe(400);
    const selfBody = (await selfResp.json()) as { error?: { code?: string } };
    expect(selfBody.error?.code).toBe('MUTEE_IS_YOURSELF');

    // delete
    const deleteResp = await callApi(request, 'renote-mute/delete', {
      i: A.token,
      userId: B.id,
    });
    expect([200, 204]).toContain(deleteResp.status());
    // 正規 path で delete 済 = afterEach cleanup の必要なし
    muterToken = undefined;
    muteeId = undefined;

    // list から消える
    const listAfter = await callApi(request, 'renote-mute/list', { i: A.token });
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as RenoteMuteEntry[];
    expect(listAfterBody.find((m) => m.muteeId === B.id)).toBeFalsy();

    // 解除済の delete は 400 + NOT_MUTING。
    const dupDelResp = await callApi(request, 'renote-mute/delete', {
      i: A.token,
      userId: B.id,
    });
    expect(dupDelResp.status()).toBe(400);
    const dupDelBody = (await dupDelResp.json()) as { error?: { code?: string } };
    expect(dupDelBody.error?.code).toBe('NOT_MUTING');
  });
});
