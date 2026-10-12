/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #819: hybrid timeline (= /api/notes/hybrid-timeline、別名 social)。
//
// upstream Misskey TS と mk-go は両方とも hybrid を:
//   - home (= followee + 自分) と local (= 同 instance の public) の merge
//   - 結果として viewer が follow していない user の public note も出る
//     (= local part 経由)
//   - 一方 home / followers visibility の non-followee 投稿は出ない
//     (= home は follow 関係が無いと届かない、local は public 限定)
//   - auth 必須 (= notes/timeline と同じく RequireAuth)
//
// 本 spec は両 backend 共通で:
//   1. viewer (A) / outsider (B) signup (follow なし)
//   2. B が public + home の 2 種類 note を投稿
//   3. A の hybrid-timeline で B の public note は出ること (local 経由)
//   4. 同 fetch で B の home note は出ないことを check (= follow 不在で
//      home visibility は届かない、boundary regression guard)

import { expect, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import {
  fetchTimelineNotes,
  pollForTimelineNote,
} from '../../../../fixtures/timeline';

test.describe('timeline: hybrid', () => {
  test.beforeEach(() => {
    resetRateLimit();
  });

  test('hybrid includes outsider public via local but excludes their home-visibility', async ({
    request,
  }) => {
    const viewer = await signupUser(request, randomUsername('hyA'));
    const outsider = await signupUser(request, randomUsername('hyB'));

    const publicNote = await createNote(request, outsider.token, {
      text: 'hybrid: public',
      visibility: 'public',
    });
    const homeNote = await createNote(request, outsider.token, {
      text: 'hybrid: home',
      visibility: 'home',
    });

    // outsider の public note は viewer の hybrid に local part 経由で
    // 出るはず (= follow 不要)。
    await pollForTimelineNote(
      request,
      'notes/hybrid-timeline',
      viewer.token,
      publicNote.id,
    );

    // home visibility の note は follow 関係が無い viewer には届かない。
    // local part にも (visibility=public 限定なので) 入らない。
    const notes = await fetchTimelineNotes(
      request,
      'notes/hybrid-timeline',
      viewer.token,
    );
    expect(notes.some((n) => n.id === homeNote.id)).toBe(false);
  });
});
