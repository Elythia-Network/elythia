/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #821 PR-B reactions spec: 同 user の 2 度付与 (1 user 1 reaction) の挙動。
//
// upstream Misskey TS と mk-go は両方とも:
//   - 同 user が同じ reaction を 2 度送ると ALREADY_REACTED で reject される
//     (= 1 user / 1 note に 1 reaction が Misskey の仕様)
//   - notes/reactions/delete で取り消した後は同じ reaction を再付与できる
//
// 本 spec は両 backend 共通で:
//   1. user A + user B signup
//   2. A が public note 投稿
//   3. B が note に `👍` 付与 → 204
//   4. B が同じ `👍` をもう一度送る → 4xx (ALREADY_REACTED 相当)
//   5. B が `notes/reactions/delete` で取り消し → 204
//   6. B が再度 `👍` 付与 → 204 (= delete 後は付与可能)
//
// 注: 違う reaction (例: `👍` の後に `❤️`) を送った場合の挙動 (= 置き換え
// vs reject) は両 backend で挙動が分かれる可能性があるため、本 spec の
// scope 外。実機で挙動を観測してから別 spec で扱う。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('reactions: same-reaction replay (1 user 1 reaction)', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('B reacts twice with the same emoji is rejected; delete + re-add succeeds', async ({
    request,
  }) => {
    const author = await signupUser(request, randomUsername('rrA'));
    const reactor = await signupUser(request, randomUsername('rrB'));

    const note = await createNote(request, author.token, {
      text: 'react replay',
      visibility: 'public',
    });

    // 1 回目の付与は成功 (204)。
    const first = await callApi(request, 'notes/reactions/create', {
      i: reactor.token,
      noteId: note.id,
      reaction: '👍',
    });
    expect(first.status()).toBeGreaterThanOrEqual(200);
    expect(first.status()).toBeLessThan(300);

    // 同じ reaction を 2 度送ると 4xx で reject される (両 backend で
    // ALREADY_REACTED 相当の error)。本 spec は status code レンジで吸収
    // (= TS=400 / mk-go=400 で揃うが、code/id strict 一致は別 spec)。
    const second = await callApi(request, 'notes/reactions/create', {
      i: reactor.token,
      noteId: note.id,
      reaction: '👍',
    });
    expect(second.status()).toBeGreaterThanOrEqual(400);
    expect(second.status()).toBeLessThan(500);

    // delete で取り消し (204)。
    const del = await callApi(request, 'notes/reactions/delete', {
      i: reactor.token,
      noteId: note.id,
    });
    expect(del.status()).toBeGreaterThanOrEqual(200);
    expect(del.status()).toBeLessThan(300);

    // delete 後は同じ reaction を再付与できる (204)。両 backend で 1 user
    // 1 reaction の lifecycle (= add → delete → add) が round-trip すること
    // を担保する。
    const third = await callApi(request, 'notes/reactions/create', {
      i: reactor.token,
      noteId: note.id,
      reaction: '👍',
    });
    expect(third.status()).toBeGreaterThanOrEqual(200);
    expect(third.status()).toBeLessThan(300);

    // 最終 state: notes/show で reactor の reaction 1 件のみ反映。
    const showResp = await callApi(request, 'notes/show', {
      i: author.token,
      noteId: note.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as { reactions: Record<string, number> };
    // 1 user 1 reaction の context なので reactions object は `👍` のみ
    // 含む。余計な key (= reactor 以外の reaction や同 reactor の多重
    // reaction) があれば drift として即 fail させる。
    expect(Object.keys(shown.reactions)).toEqual(['👍']);
    expect(shown.reactions['👍']).toBe(1);
  });
});
