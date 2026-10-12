/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #821 PR-C reactions spec: 違う reaction を 2 度目に送ったときの挙動。
//
// PR-B (#863) で同じ reaction を 2 度送ると ALREADY_REACTED で reject される
// ことを spec 化した。本 spec はその延長として、違う reaction (例: `👍` の
// 後に `❤️`) を送った時の挙動を両 backend で観察し共通仕様を確認した:
//
//   - 両 backend ともに 204 で受け入れ (= 置き換え挙動)
//   - 1 個目の reaction (`👍`) は unset され、2 個目 (heart) のみ残る
//   - reactions object は 1 key / count=1 になる
//
// emoji variation selector の正規化 (#864 で fix 済) により、両 backend
// ともに reaction 文字列を `'❤'` (variation selector strip) で保存する。
// 本 spec は heart の reaction key を `'❤'` で strict 比較する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('reactions: different-reaction replay (replaced, not duplicated)', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('B reacts with thumbs-up then heart; first is unset, only heart remains', async ({
    request,
  }) => {
    const author = await signupUser(request, randomUsername('rdA'));
    const reactor = await signupUser(request, randomUsername('rdB'));

    const note = await createNote(request, author.token, {
      text: 'react diff',
      visibility: 'public',
    });

    // 1 個目: `👍` 付与 → 204。
    const first = await callApi(request, 'notes/reactions/create', {
      i: reactor.token,
      noteId: note.id,
      reaction: '👍',
    });
    expect(first.status()).toBeGreaterThanOrEqual(200);
    expect(first.status()).toBeLessThan(300);

    // 2 個目: 違う `❤️` を送る → 両 backend で 204 で受け入れ (置き換え)。
    const second = await callApi(request, 'notes/reactions/create', {
      i: reactor.token,
      noteId: note.id,
      reaction: '❤️',
    });
    expect(second.status()).toBeGreaterThanOrEqual(200);
    expect(second.status()).toBeLessThan(300);

    // 最終 state: 1 個目の `👍` は unset、2 個目の heart のみ残る。
    const showResp = await callApi(request, 'notes/show', {
      i: author.token,
      noteId: note.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as { reactions: Record<string, number> };
    // 1 reactor / 1 reaction なので reactions object は 1 key のみ。
    // 両 backend で variation selector を strip した `'❤'` で保存される
    // (#864 fix 後)。Object.keys 全体を strict 比較することで余計な key の
    // drift と heart の表記揺れを同時に検出する。
    expect(Object.keys(shown.reactions)).toEqual(['❤']);
    expect(shown.reactions['❤']).toBe(1);
  });
});
