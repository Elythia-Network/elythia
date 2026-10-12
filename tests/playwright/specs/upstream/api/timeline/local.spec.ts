/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #819: local timeline (= /api/notes/local-timeline)。
//
// upstream Misskey TS と mk-go は両方とも:
//   - 同 instance の user による public visibility note のみ返す
//     (DB 側で `userHost IS NULL AND visibility = 'public'` で filter)
//   - followers / specified visibility の note は出ない
//   - home visibility の note は出ない (= local timeline は home を含めない)
//   - channel 内 note も出ない (channelId IS NULL で除外)
//
// 本 spec は両 backend 共通で:
//   1. author / viewer signup
//   2. author が public + followers の 2 種類 note を投稿
//   3. viewer の local-timeline で public は出る、followers は出ないことを
//      check (visibility filter regression guard)

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import {
  fetchTimelineNotes,
  pollForTimelineNote,
} from '../../../../fixtures/timeline';

test.describe('timeline: local', () => {
  test.beforeEach(() => {
    resetRateLimit();
  });

  test('local includes public but excludes followers-only', async ({
    request,
  }) => {
    const author = await signupUser(request, randomUsername('ltA'));
    const viewer = await signupUser(request, randomUsername('ltB'));

    const publicNote = await createNote(request, author.token, {
      text: 'local: public',
      visibility: 'public',
    });
    const followersNote = await createNote(request, author.token, {
      text: 'local: followers',
      visibility: 'followers',
    });

    // public note が viewer の local-timeline に到達するまで poll。
    await pollForTimelineNote(
      request,
      'notes/local-timeline',
      viewer.token,
      publicNote.id,
    );

    // followers visibility の note は viewer (= follower でない 第三者)
    // からは見えない。fanout settle 後の同 fetch で確認する。
    const notes = await fetchTimelineNotes(
      request,
      'notes/local-timeline',
      viewer.token,
    );
    expect(notes.some((n) => n.id === followersNote.id)).toBe(false);
  });
});
