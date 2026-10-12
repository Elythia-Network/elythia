/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #836: i/favorites round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - notes/favorites/create で note を favorite する (204)
//   - i/favorites で自分の favorite した note 一覧を取得 (NoteFavorite 配列)
//   - notes/favorites/delete で解除 (204)
//
// favorite list の entry は { id, createdAt, noteId, note: NoteDetailed }
// shape (= NoteFavorite)。spec では noteId / note.id 整合のみ verify。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface FavoriteEntry {
  id: string;
  createdAt: string;
  noteId: string;
  note?: { id: string };
}

test.describe('i/favorites: create / list / delete round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('favorite a note then unfavorite reflects via i/favorites', async ({ request }) => {
    const me = await signupUser(request, randomUsername('favA'));
    const note = await createNote(request, me.token, {
      text: 'favorite target',
      visibility: 'public',
    });

    // create favorite
    const createResp = await callApi(request, 'notes/favorites/create', {
      i: me.token,
      noteId: note.id,
    });
    expect([200, 204]).toContain(createResp.status());

    // i/favorites に含まれる
    const listResp = await callApi(request, 'i/favorites', { i: me.token });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as FavoriteEntry[];
    expect(Array.isArray(list)).toBe(true);
    const found = list.find((f) => f.noteId === note.id);
    expect(found).toBeDefined();
    // note field が embed されている (= NoteFavorite shape)
    expect(found?.note?.id).toBe(note.id);

    // delete favorite
    const deleteResp = await callApi(request, 'notes/favorites/delete', {
      i: me.token,
      noteId: note.id,
    });
    expect([200, 204]).toContain(deleteResp.status());

    // i/favorites から消える
    const listAfter = await callApi(request, 'i/favorites', { i: me.token });
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as FavoriteEntry[];
    expect(listAfterBody.find((f) => f.noteId === note.id)).toBeFalsy();
  });
});
