/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// リバーシの対局の操作を REST で行う独自の API (#3942) を叩き、応答を /api.json の宣言で
// 照合する (#4093)。本家ではどれも reversiGame のチャンネルのメッセージで、REST には無い。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { callChecked } from '../../../fixtures/api_schema';
import { type Principal, randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';

interface Game { id: string; isStarted: boolean; isEnded: boolean; black: number | null; user1Id: string; logs: unknown[] }

// 既定の盤 (internal/api/reversi/handler.go の 8x8) で、先手の黒が置ける所の 1 つ。
// 真下の w (27) を挟んで、その下の b (35) へ続く (位置は左上を 0 とした行優先の番号)
const FIRST_MOVE = 19;

/** Makes a game between a and b: each names the other (reversi/match). */
async function makeGame(request: Parameters<typeof callApi>[0], a: Principal, b: Principal): Promise<string> {
  expect((await callApi(request, 'reversi/match', { i: a.token, userId: b.id })).status()).toBe(204);
  const resp = await callApi(request, 'reversi/match', { i: b.token, userId: a.id });
  expect(resp.status(), await resp.text()).toBe(200);
  return ((await resp.json()) as { id: string }).id;
}

async function showGame(request: Parameters<typeof callApi>[0], gameId: string): Promise<Game> {
  const resp = await callApi(request, 'reversi/show-game', { gameId });
  expect(resp.status(), await resp.text()).toBe(200);
  return (await resp.json()) as Game;
}

test.describe('reversi: the REST actions of a game', () => {
  test.skip(isTsBackend, 'reversi/ready・put-stone・claim-time-is-up は Elythia 独自');

  test.beforeAll(() => {
    resetRateLimit();
  });

  test('both get ready, black puts a stone, and a claim of time up while time is left does nothing', async ({ request }) => {
    const a = await signupUser(request, randomUsername('rvra'));
    const b = await signupUser(request, randomUsername('rvrb'));
    const gameId = await makeGame(request, a, b);

    await callChecked(request, 'reversi/ready', { i: a.token, gameId, ready: true });
    expect((await showGame(request, gameId)).isStarted).toBe(false);
    await callChecked(request, 'reversi/ready', { i: b.token, gameId, ready: true });
    const started = await showGame(request, gameId);
    expect(started.isStarted).toBe(true);
    expect([1, 2]).toContain(started.black);

    const black = (started.user1Id === a.id) === (started.black === 1) ? a : b;
    await callChecked(request, 'reversi/put-stone', { i: black.token, gameId, pos: FIRST_MOVE, id: 'pw-first' });
    expect((await showGame(request, gameId)).logs.length).toBe(1);
    // 置けたのは黒の番だったから。続けて置こうとすると、白の番なので断られる
    const again = await callApi(request, 'reversi/put-stone', { i: black.token, gameId, pos: 26 });
    expect(((await again.json()) as { error: { code: string } }).error.code).toBe('NOT_YOUR_TURN');

    // 手番の時間が残っているので、何も起きない
    await callChecked(request, 'reversi/claim-time-is-up', { i: a.token, gameId });
    expect((await showGame(request, gameId)).isEnded).toBe(false);
  });
});
