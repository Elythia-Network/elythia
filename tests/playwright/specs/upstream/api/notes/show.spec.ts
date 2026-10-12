/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1 PR-3: notes/show の正常系 + 不存在 note 経路。
// 作成した public note が notes/show で取得でき、適当な ID では 4xx を返す
// ことを確認する。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote, showNoteRaw } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('notes: show', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('show returns the note that was just created', async ({ request }) => {
    const username = randomUsername('nSh');
    const me = await signupUser(request, username);
    const note = await createNote(request, me.token, {
      text: 'visible to everyone',
      visibility: 'public',
    });

    const resp = await showNoteRaw(request, me.token, note.id);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.id).toBe(note.id);
    expect(body.text).toBe('visible to everyone');
    expect(body.userId).toBe(me.id);
  });

  test('public note is visible without auth too', async ({ request }) => {
    const username = randomUsername('nShAn');
    const me = await signupUser(request, username);
    const note = await createNote(request, me.token, {
      text: 'world readable',
      visibility: 'public',
    });

    // anonymous viewer (= token なし)。upstream Misskey TS は public note を
    // 未認証でも返す。mk-go の visibility filter が同様の挙動を取ること。
    const resp = await showNoteRaw(request, null, note.id);
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.id).toBe(note.id);
  });

  test('non-existent noteId returns 4xx', async ({ request }) => {
    const username = randomUsername('nSh404');
    const me = await signupUser(request, username);

    const resp = await showNoteRaw(request, me.token, 'this-note-does-not-exist');
    expect(resp.status()).toBeGreaterThanOrEqual(400);
    expect(resp.status()).toBeLessThan(500);
  });
});
