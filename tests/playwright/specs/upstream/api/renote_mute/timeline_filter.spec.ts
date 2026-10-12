/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #903: renote-mute timeline filter integration spec。
//
// upstream Misskey TS と mk-go (本 PR で実装) は両方とも:
//   - renote-mute は **pure renote のみ** を timeline から除外する
//   - 投稿者の plain note / quote renote (text or file 付き) は通す
//
// scenario:
//   - A が B を renote-mute
//   - C が public note を作成
//   - B が C の note を pure renote (text なし)
//   - B が plain note (= renote 以外) を作成
//   - A の local timeline:
//       * C の元 note → 含まれる
//       * B の plain note → 含まれる
//       * B の pure renote → **含まれない**
//
// upstream Misskey TS の generateMutedUserRelatedRenotesQuery と同 semantics。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface TimelineNote {
  id: string;
  userId: string;
  text: string | null;
  renoteId?: string | null;
}

test.describe('renote-mute: timeline filter integration', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('pure renote by renote-muted user is filtered, plain note is kept', async ({
    request,
  }) => {
    const A = await signupUser(request, randomUsername('rmtA'));
    const B = await signupUser(request, randomUsername('rmtB'));
    const C = await signupUser(request, randomUsername('rmtC'));

    // A renote-mutes B
    const rmCreate = await callApi(request, 'renote-mute/create', {
      i: A.token,
      userId: B.id,
    });
    expect([200, 204]).toContain(rmCreate.status());

    // C creates a public note
    const cNote = await createNote(request, C.token, {
      text: 'note from C',
      visibility: 'public',
    });

    // B pure-renotes C's note (text なし、file なし、renoteId のみ)
    const bRenote = await createNote(request, B.token, {
      visibility: 'public',
      renoteId: cNote.id,
    });

    // B creates a plain note (= renote-mute 対象外であるべき)
    const bPlain = await createNote(request, B.token, {
      text: 'B plain note',
      visibility: 'public',
    });

    // A の local timeline を取得
    const tlResp = await callApi(request, 'notes/local-timeline', {
      i: A.token,
      limit: 100,
    });
    expect(tlResp.status()).toBe(200);
    const tl = (await tlResp.json()) as TimelineNote[];

    const ids = new Set(tl.map((n) => n.id));
    expect(ids.has(cNote.id)).toBe(true);
    expect(ids.has(bPlain.id)).toBe(true);
    expect(ids.has(bRenote.id)).toBe(false);

    // cleanup: A の renote-mute を解除 (assertion 失敗時の orphan 防止)
    await callApi(request, 'renote-mute/delete', {
      i: A.token,
      userId: B.id,
    });
  });
});
