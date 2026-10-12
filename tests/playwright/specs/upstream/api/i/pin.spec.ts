/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #836: i/pin + i/unpin round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - i/pin { noteId } で自分の note を pin (= profile に表示する固定 note、
//     200 + updated UserDetailed)
//   - users/show { userId } で pinnedNoteIds に id が含まれる
//   - i/unpin { noteId } で解除 (200 + updated UserDetailed)
//
// pin 上限 (5 件) / 既 pin / 他人の note 等の error path は別 spec scope。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface UserDetailed {
  id: string;
  pinnedNoteIds?: string[];
  pinnedNotes?: Array<{ id: string }>;
}

test.describe('i/pin + i/unpin round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('pin a note then unpin reflects via users/show', async ({ request }) => {
    const me = await signupUser(request, randomUsername('pinA'));
    const note = await createNote(request, me.token, {
      text: 'pin target',
      visibility: 'public',
    });

    // pin
    const pinResp = await callApi(request, 'i/pin', {
      i: me.token,
      noteId: note.id,
    });
    expect(pinResp.status()).toBe(200);

    // users/show で pinnedNoteIds (or pinnedNotes) に含まれる
    const showResp = await callApi(request, 'users/show', {
      i: me.token,
      userId: me.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as UserDetailed;
    // backend で field name 微差を吸収する LCD: pinnedNoteIds (mk-go) /
    // pinnedNotes[].id (upstream TS detail) のいずれかに含まれることを確認。
    const pinned = new Set<string>([
      ...(shown.pinnedNoteIds ?? []),
      ...((shown.pinnedNotes ?? []).map((n) => n.id)),
    ]);
    expect(pinned.has(note.id)).toBe(true);

    // unpin
    const unpinResp = await callApi(request, 'i/unpin', {
      i: me.token,
      noteId: note.id,
    });
    expect(unpinResp.status()).toBe(200);

    // unpin 後 pinnedNoteIds から消える
    const showAfter = await callApi(request, 'users/show', {
      i: me.token,
      userId: me.id,
    });
    expect(showAfter.status()).toBe(200);
    const shownAfter = (await showAfter.json()) as UserDetailed;
    const pinnedAfter = new Set<string>([
      ...(shownAfter.pinnedNoteIds ?? []),
      ...((shownAfter.pinnedNotes ?? []).map((n) => n.id)),
    ]);
    expect(pinnedAfter.has(note.id)).toBe(false);
  });
});
