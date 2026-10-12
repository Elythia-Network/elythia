/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1 PR-3: notes/create の正常系。
// signupUser で fresh user を作り、notes/create で public note を投稿、
// 戻り response が upstream Misskey TS と整合する shape (= id, text, userId,
// visibility) を持つことを確認する。

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('notes: create', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('creates a public note and returns the expected shape', async ({ request }) => {
    const username = randomUsername('nCre');
    const me = await signupUser(request, username);

    const note = await createNote(request, me.token, {
      text: 'hello from playwright',
      visibility: 'public',
    });

    expect(note.id).toBeTruthy();
    expect(note.text).toBe('hello from playwright');
    expect(note.userId).toBe(me.id);
    expect(note.visibility).toBe('public');
  });

  test('respects cw (content warning) when supplied', async ({ request }) => {
    const username = randomUsername('nCw');
    const me = await signupUser(request, username);

    const note = await createNote(request, me.token, {
      text: 'spoiler body',
      cw: 'spoiler ahead',
      visibility: 'public',
    });
    expect(note.cw).toBe('spoiler ahead');
    expect(note.text).toBe('spoiler body');
  });
});
