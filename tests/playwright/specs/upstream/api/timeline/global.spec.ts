/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #819: global timeline (= /api/notes/global-timeline)。
//
// upstream Misskey TS と mk-go は両方とも:
//   - 全 instance の public visibility note を返す
//     (DB 側で `visibility = 'public' AND channelId IS NULL` で filter)
//   - followers / specified / home visibility は出ない (= public 以外は除外)
//
// single-instance environment では federated remote が無いので、global は
// 実質 local の super-set として動く (local は userHost IS NULL 制限あり)。
// 本 spec は visibility filter の境界に focus し:
//   1. author / viewer signup
//   2. author が public + home の 2 種類 note を投稿
//   3. viewer の global-timeline で public は出る、home は出ないことを check

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import {
  fetchTimelineNotes,
  pollForTimelineNote,
} from '../../../../fixtures/timeline';

test.describe('timeline: global', () => {
  test.beforeEach(() => {
    resetRateLimit();
  });

  test('global includes public but excludes home-visibility', async ({
    request,
  }) => {
    const author = await signupUser(request, randomUsername('gtA'));
    const viewer = await signupUser(request, randomUsername('gtB'));

    const publicNote = await createNote(request, author.token, {
      text: 'global: public',
      visibility: 'public',
    });
    const homeNote = await createNote(request, author.token, {
      text: 'global: home',
      visibility: 'home',
    });

    await pollForTimelineNote(
      request,
      'notes/global-timeline',
      viewer.token,
      publicNote.id,
    );

    // home visibility は public timeline 系から除外される (upstream TS
    // と mk-go DB 側で同 SQL filter)。fanout settle 後の同 fetch で
    // 確認する。
    const notes = await fetchTimelineNotes(
      request,
      'notes/global-timeline',
      viewer.token,
    );
    expect(notes.some((n) => n.id === homeNote.id)).toBe(false);
  });
});
