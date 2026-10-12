/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #819: home timeline (= /api/notes/timeline) の包含 / 除外境界。
//
// upstream Misskey TS と mk-go は両方とも:
//   - viewer 自身 + viewer が follow する user の note のみが home に出る
//   - viewer が follow していない user の public note は home に出ない
//   - auth 必須 (= viewer なしは 401 CREDENTIAL_REQUIRED)
//
// 本 spec は両 backend 共通で:
//   1. follower (A) / followee (B) / outsider (C) signup
//   2. A が B を follow
//   3. B / C それぞれ public note を投稿
//   4. A の home timeline で B の note は出る、C の note は出ないことを check
//   5. 未認証 request は 401 で reject されること
//
// channel / list / antenna / hashtag / role timeline は別 spec scope (#819
// issue で明記済み)。本 PR では 4 種 (home/local/global/hybrid) のみ。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { resetRateLimit } from '../../../../fixtures/rate_limit';
import {
  fetchTimelineNotes,
  pollForTimelineNote,
} from '../../../../fixtures/timeline';

test.describe('timeline: home', () => {
  // 各 test で 3 人 signup するので spec 累積で signup rate limit (1h 5回) に
  // 引っかかる。test 境界で reset して独立性を保つ。
  test.beforeEach(() => {
    resetRateLimit();
  });

  test('home includes followed user but excludes outsider', async ({
    request,
  }) => {
    const follower = await signupUser(request, randomUsername('htA'));
    const followee = await signupUser(request, randomUsername('htB'));
    const outsider = await signupUser(request, randomUsername('htC'));

    // follower → followee の follow を確立 (= followee の note が
    // follower の home fanout に乗る前提)。
    const fResp = await callApi(request, 'following/create', {
      i: follower.token,
      userId: followee.id,
    });
    expect(fResp.status()).toBeGreaterThanOrEqual(200);
    expect(fResp.status()).toBeLessThan(300);

    const followedNote = await createNote(request, followee.token, {
      text: 'home: followee',
      visibility: 'public',
    });
    const outsiderNote = await createNote(request, outsider.token, {
      text: 'home: outsider',
      visibility: 'public',
    });

    // followee の note が follower の home に届くまで poll で待つ。
    await pollForTimelineNote(
      request,
      'notes/timeline',
      follower.token,
      followedNote.id,
    );

    // poll が通った時点で fanout は settle 済み。同 fetch で outsider の
    // note は含まれないことを check (= follow していない user の note は
    // home に来ない、boundary regression guard)。
    const notes = await fetchTimelineNotes(
      request,
      'notes/timeline',
      follower.token,
    );
    expect(notes.some((n) => n.id === outsiderNote.id)).toBe(false);
  });

  test('home requires auth', async ({ request }) => {
    // upstream Misskey TS と mk-go は両方とも notes/timeline を
    // RequireAuth() で wrap しており、token 無しは 401 を返す。
    const resp = await callApi(request, 'notes/timeline', {});
    expect(resp.status()).toBe(401);
  });
});
