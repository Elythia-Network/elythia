/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// バブルゲームの対戦 (#3228) の独自の API を叩き、応答を /api.json の宣言で照合する (#4093)。
// 準備完了は REST に無く、対戦のチャンネルで送る (fixtures/route_seed.ts の startVersus)。

import { expect, test } from '@playwright/test';
import { callChecked } from '../../../fixtures/api_schema';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { startVersus } from '../../../fixtures/route_seed';

interface Match { id: string; status: string; user1Id: string; user2Id: string }
interface MatchRecord { id: string; user1Id: string; user1Public: boolean; user2Public: boolean }

test.describe('bubble game versus: the API answers as declared', () => {
  test.skip(isTsBackend, 'バブルゲームの対戦は Elythia 独自');

  test.beforeAll(() => {
    resetRateLimit();
  });

  test('invite, see the invitation, accept and show', async ({ request }) => {
    const a = await signupUser(request, randomUsername('bvia'));
    const b = await signupUser(request, randomUsername('bvib'));
    const invited = await callChecked<Match>(request, 'bubble-game/versus/invite', { i: a.token, userId: b.id, gameMode: 'normal' });
    expect(invited.status).toBe('invited');
    const invitations = await callChecked<Match[]>(request, 'bubble-game/versus/invitations', { i: b.token });
    expect(invitations.map((m) => m.id)).toContain(invited.id);
    const accepted = await callChecked<Match>(request, 'bubble-game/versus/accept', { i: b.token, matchId: invited.id });
    expect(accepted.status).toBe('accepted');
    const shown = await callChecked<Match>(request, 'bubble-game/versus/show', { i: a.token, matchId: invited.id });
    expect(shown.id).toBe(invited.id);
    await callChecked(request, 'bubble-game/versus/cancel', { i: a.token, matchId: invited.id });
  });

  test('decline an invitation, and cancel one', async ({ request }) => {
    const a = await signupUser(request, randomUsername('bvda'));
    const b = await signupUser(request, randomUsername('bvdb'));
    const first = await callChecked<Match>(request, 'bubble-game/versus/invite', { i: a.token, userId: b.id, gameMode: 'normal' });
    await callChecked(request, 'bubble-game/versus/decline', { i: b.token, matchId: first.id });
    const second = await callChecked<Match>(request, 'bubble-game/versus/invite', { i: a.token, userId: b.id, gameMode: 'normal' });
    await callChecked(request, 'bubble-game/versus/cancel', { i: a.token, matchId: second.id });
    expect(await callChecked<Match[]>(request, 'bubble-game/versus/invitations', { i: b.token })).toEqual([]);
  });

  test('play to the end, then read the record and the history and make it public', async ({ request }) => {
    const a = await signupUser(request, randomUsername('bvpa'));
    const b = await signupUser(request, randomUsername('bvpb'));
    const matchId = await startVersus(request, a, b);
    const reported = await callChecked<Match>(request, 'bubble-game/versus/report', { i: a.token, matchId, score: 120, frame: 600, reason: 'gameOver', logs: [] });
    expect(reported.id).toBe(matchId);
    const ended = await callChecked<Match>(request, 'bubble-game/versus/report', { i: b.token, matchId, score: 80, frame: 600, reason: 'opponentEnded', logs: [] });
    expect(ended.status).toBe('ended');

    const record = await callChecked<MatchRecord>(request, 'bubble-game/versus/record', { i: a.token, matchId });
    expect(record.id).toBe(matchId);
    await callChecked(request, 'bubble-game/versus/set-public', { i: a.token, matchId, isPublic: true });
    const history = await callChecked<MatchRecord[]>(request, 'bubble-game/versus/history', { i: a.token });
    const mine = history.find((r) => r.id === matchId);
    expect(mine, 'the history has the match').toBeDefined();
    expect(mine!.user1Id === a.id ? mine!.user1Public : mine!.user2Public).toBe(true);
  });
});
