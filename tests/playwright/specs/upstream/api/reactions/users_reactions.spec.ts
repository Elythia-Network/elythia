/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #821 PR-D reactions spec: users/reactions で reactor 視点の reaction list
// round-trip。
//
// upstream Misskey TS と mk-go (#821 PR-D で本実装に修正) は両方とも:
//   - users/reactions { userId } で指定 user が付けた reaction の list を
//     返す
//   - target user の publicReactions が false で viewer ≠ target なら
//     403 REACTIONS_NOT_PUBLIC
//   - self view (viewer === target) なら publicReactions に関わらず取得可能
//   - response の各 entry は { id, createdAt, type, user, note } の minimal
//     shape
//
// 本 spec は両 backend 共通で:
//   1. user A + user B signup
//   2. A が public note 投稿
//   3. B が note に `👍` reaction 付与
//   4. B が users/reactions { userId: B.id } で self view 取得
//   5. list 1 件 / type / user.id / note.id を strict assert
//
// 注: viewer ≠ target 視点の publicReactions check は別 spec で扱う scope
// (本 spec は self view round-trip にフォーカス)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface UsersReactionsEntry {
  id: string;
  createdAt: string;
  type: string;
  user: { id: string };
  note: { id: string };
}

test.describe('reactions: users/reactions self view round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('B reacts to A\'s note then sees the reaction in B\'s users/reactions (self view)', async ({
    request,
  }) => {
    const author = await signupUser(request, randomUsername('urA'));
    const reactor = await signupUser(request, randomUsername('urB'));

    const note = await createNote(request, author.token, {
      text: 'react for users/reactions',
      visibility: 'public',
    });

    // reactor が `👍` 付与。
    const reactResp = await callApi(request, 'notes/reactions/create', {
      i: reactor.token,
      noteId: note.id,
      reaction: '👍',
    });
    expect(reactResp.status()).toBeGreaterThanOrEqual(200);
    expect(reactResp.status()).toBeLessThan(300);

    // reactor が自分の users/reactions を取得 (self view なので
    // publicReactions に関わらず取得可能)。
    const listResp = await callApi(request, 'users/reactions', {
      i: reactor.token,
      userId: reactor.id,
      limit: 10,
    });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as UsersReactionsEntry[];
    expect(Array.isArray(list)).toBe(true);
    // self の reaction は 1 件のみ (= 直前に付けた `👍`)。
    expect(list.length).toBe(1);
    const entry = list[0];
    expect(entry.type).toBe('👍');
    expect(entry.user.id).toBe(reactor.id);
    expect(entry.note.id).toBe(note.id);
    expect(Number.isFinite(Date.parse(entry.createdAt))).toBe(true);
  });
});
