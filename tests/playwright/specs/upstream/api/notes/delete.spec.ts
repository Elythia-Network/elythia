/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1 PR-3: notes/delete の正常系 + 他人 note の削除を拒否する
// ことを確認する。upstream Misskey TS は他人 note の削除を ACCESS_DENIED
// 系の error で reject する shape。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote, deleteNote, showNoteRaw } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('notes: delete', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('owner can delete their own note', async ({ request }) => {
    const username = randomUsername('nDel');
    const me = await signupUser(request, username);
    const note = await createNote(request, me.token, {
      text: 'will be deleted',
      visibility: 'public',
    });

    // upstream は 204 No Content を返す。mk-go も同 status を期待。
    const delResp = await deleteNote(request, me.token, note.id);
    expect(delResp.status()).toBe(204);

    // 削除後は notes/show で 4xx (= NO_SUCH_NOTE 相当)。
    const showResp = await showNoteRaw(request, me.token, note.id);
    expect(showResp.status()).toBeGreaterThanOrEqual(400);
    expect(showResp.status()).toBeLessThan(500);
  });

  test('non-owner cannot delete another user note', async ({ request }) => {
    const owner = await signupUser(request, randomUsername('nDelOw'));
    const stranger = await signupUser(request, randomUsername('nDelSt'));

    const note = await createNote(request, owner.token, {
      text: 'mine to keep',
      visibility: 'public',
    });

    // 別 user の token で削除を試みる → 4xx で reject。
    const delResp = await deleteNote(request, stranger.token, note.id);
    expect(delResp.status()).toBeGreaterThanOrEqual(400);
    expect(delResp.status()).toBeLessThan(500);

    // note 自体は依然として存在する (owner から show で取れる)。
    const showResp = await showNoteRaw(request, owner.token, note.id);
    expect(showResp.status()).toBe(200);
  });
});
