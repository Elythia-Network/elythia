/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #821 Phase 2 reactions spec: reactions create / delete + notes/show reflect の
// round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - notes/reactions/create { noteId, reaction } で reaction を付与 (204)
//   - 付与後 notes/show で取得した note.reactions に reaction が反映される
//     (= `{ "<reaction>": <count>, ... }` の object 形式)
//   - notes/reactions { noteId } で reaction list (id / createdAt / user / type) を
//     取得できる
//   - notes/reactions/delete { noteId } で自分の reaction を取り消す (204)
//   - 削除後 notes/show で reactions object から消える
//
// 本 spec は両 backend 共通で:
//   1. user A + user B signup
//   2. A が public note 投稿
//   3. B が note に reaction 付与 (👍)
//   4. notes/show で取得した note.reactions に `{ "👍": 1 }` が反映
//   5. notes/reactions で reactor list に B が含まれる
//   6. B が reaction 取り消し
//   7. notes/show で取得した note.reactions から `👍` が消える
//
// reaction notification (= 受信側の WS / /api/i/notifications 反映) は
// reaction.spec.ts (#847) で既に round-trip 検証済み。本 spec は reaction
// 自体の create / delete / list / notes/show 反映に focus する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface ReactionListEntry {
  id: string;
  createdAt: string;
  type: string;
  user: { id: string };
}

test.describe('reactions: create / delete round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('B reacts to A\'s note, listed in reactions, removed after delete', async ({
    request,
  }) => {
    const author = await signupUser(request, randomUsername('rxA'));
    const reactor = await signupUser(request, randomUsername('rxB'));

    // author が public note 投稿。
    const note = await createNote(request, author.token, {
      text: 'react to me',
      visibility: 'public',
    });

    // reactor が reaction 付与 (= 標準 unicode emoji で deterministic に)。
    const reactResp = await callApi(request, 'notes/reactions/create', {
      i: reactor.token,
      noteId: note.id,
      reaction: '👍',
    });
    expect(reactResp.status()).toBeGreaterThanOrEqual(200);
    expect(reactResp.status()).toBeLessThan(300);

    // notes/show で reactions object に反映されることを assert。
    const showResp = await callApi(request, 'notes/show', {
      i: author.token,
      noteId: note.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as { reactions: Record<string, number> };
    // reactions field は両 backend で `{ "<reaction>": <count> }` の object。
    // unicode emoji は文字列のまま key として扱われる。
    expect(shown.reactions['👍']).toBe(1);

    // notes/reactions で reactor list 取得。
    const listResp = await callApi(request, 'notes/reactions', {
      i: author.token,
      noteId: note.id,
      limit: 10,
    });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as ReactionListEntry[];
    expect(Array.isArray(list)).toBe(true);
    // 1 reactor / 1 reaction の context なので list は厳密に 1 件。両
    // backend で余計な entry や欠落 drift があれば即 fail する。
    expect(list.length).toBe(1);
    const found = list[0];
    expect(found.user.id).toBe(reactor.id);
    // type は upstream / mk-go ともに reaction string をそのまま返す。
    // unicode emoji は raw、custom emoji は `:name@.:` 形式 (本 spec では
    // unicode のみ assert)。
    expect(found.type).toBe('👍');
    expect(Number.isFinite(Date.parse(found.createdAt))).toBe(true);

    // reactor が reaction 取り消し。
    const delResp = await callApi(request, 'notes/reactions/delete', {
      i: reactor.token,
      noteId: note.id,
    });
    expect(delResp.status()).toBeGreaterThanOrEqual(200);
    expect(delResp.status()).toBeLessThan(300);

    // notes/show で reactions object から消えていることを assert。
    const afterResp = await callApi(request, 'notes/show', {
      i: author.token,
      noteId: note.id,
    });
    expect(afterResp.status()).toBe(200);
    const after = (await afterResp.json()) as { reactions: Record<string, number> };
    // 取り消し後は reactions object に該当 key が無い。upstream / mk-go と
    // もに 0 件 reaction の場合 key 自体を omit する想定。
    expect(after.reactions['👍']).toBeUndefined();
  });
});
