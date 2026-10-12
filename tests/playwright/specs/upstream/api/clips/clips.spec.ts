/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #825: clips CRUD + add-note / remove-note round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - /api/clips/create で name + isPublic で clip を作成
//   - /api/clips/show / list で取得
//   - /api/clips/update で name / description を更新
//   - /api/clips/add-note で note を clip に追加 (= clip-membership 追加)
//   - /api/clips/notes で clip 内の note 一覧 (= add-note 後に出現)
//   - /api/clips/remove-note で remove
//   - /api/clips/delete で削除
//
// notes/create は wrapper の createdNote を返すため、note を作って add する
// 経路で一連の挙動を担保する (#860 で wrapper shape は確認済)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('clips: CRUD + add-note / remove-note round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('create / show / update / add-note / remove-note / delete', async ({ request }) => {
    const me = await signupUser(request, randomUsername('clA'));

    // create clip
    const createResp = await callApi(request, 'clips/create', {
      i: me.token,
      name: 'My Clip',
      description: 'desc',
      isPublic: true,
    });
    expect(createResp.status()).toBe(200);
    const clip = await createResp.json();
    expect(typeof clip.id).toBe('string');
    expect(clip.userId).toBe(me.id);
    expect(clip.name).toBe('My Clip');
    expect(clip.isPublic).toBe(true);

    // show
    const showResp = await callApi(request, 'clips/show', {
      i: me.token,
      clipId: clip.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.id).toBe(clip.id);
    expect(shown.name).toBe('My Clip');

    // list で自分の clip が含まれる
    const listResp = await callApi(request, 'clips/list', { i: me.token });
    expect(listResp.status()).toBe(200);
    const list = await listResp.json();
    expect(Array.isArray(list)).toBe(true);
    expect(list.find((c: { id: string }) => c.id === clip.id)).toBeTruthy();

    // update
    const updateResp = await callApi(request, 'clips/update', {
      i: me.token,
      clipId: clip.id,
      name: 'Updated Clip',
      description: 'new desc',
      isPublic: false,
    });
    expect([200, 204]).toContain(updateResp.status());

    const showAfterUpdate = await callApi(request, 'clips/show', {
      i: me.token,
      clipId: clip.id,
    });
    expect(showAfterUpdate.status()).toBe(200);
    const updated = await showAfterUpdate.json();
    expect(updated.name).toBe('Updated Clip');
    expect(updated.isPublic).toBe(false);

    // note を作って clip に add
    const note = await createNote(request, me.token, {
      text: 'note for clip',
      visibility: 'public',
    });
    const addResp = await callApi(request, 'clips/add-note', {
      i: me.token,
      clipId: clip.id,
      noteId: note.id,
    });
    expect([200, 204]).toContain(addResp.status());

    // /api/clips/notes に note が含まれる
    const notesResp = await callApi(request, 'clips/notes', {
      i: me.token,
      clipId: clip.id,
    });
    expect(notesResp.status()).toBe(200);
    const notes = await notesResp.json();
    expect(Array.isArray(notes)).toBe(true);
    expect(notes.find((n: { id: string }) => n.id === note.id)).toBeTruthy();

    // remove-note 後は notes から消える
    const removeResp = await callApi(request, 'clips/remove-note', {
      i: me.token,
      clipId: clip.id,
      noteId: note.id,
    });
    expect([200, 204]).toContain(removeResp.status());

    const notesAfterRemove = await callApi(request, 'clips/notes', {
      i: me.token,
      clipId: clip.id,
    });
    expect(notesAfterRemove.status()).toBe(200);
    const notesAfter = await notesAfterRemove.json();
    expect(notesAfter.find((n: { id: string }) => n.id === note.id)).toBeFalsy();

    // delete clip
    const deleteResp = await callApi(request, 'clips/delete', {
      i: me.token,
      clipId: clip.id,
    });
    expect([200, 204]).toContain(deleteResp.status());

    // delete 後の show は 4xx
    const showAfterDelete = await callApi(request, 'clips/show', {
      i: me.token,
      clipId: clip.id,
    });
    expect(showAfterDelete.status()).toBeGreaterThanOrEqual(400);
    expect(showAfterDelete.status()).toBeLessThan(500);
  });
});
