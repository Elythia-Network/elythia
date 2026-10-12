/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #837 PR-B: reversi multiplayer round-trip。
//
// upstream Misskey TS と mk-go は両方とも 2 user 間の reversi game state を
// HTTP API で操作できる。本 spec は shape + status 互換に絞る LCD strategy:
//
//   - reversi/games: anonymous 可、completed game 一覧 (= 配列)
//   - reversi/match: 招待を投函。upstream は 204 / mk-go は 200+body の
//     drift がある (#898 wontfix、両 backend で `[200, 204]` 許容)
//   - reversi/cancel-match: 自分の pending invitation を一括 cancel → 204
//   - reversi/show-game: gameId 必須、unknown は 4xx (TS 400 / mk-go 404 LCD)
//   - reversi/invitations: viewer 宛 pending 一覧 (= UserLite 配列)。
//     cancel 後の cleanup は backend 間で挙動差あり (#899 wontfix)、本 spec
//     では cancel 後の disappearance を strict-assert しない LCD 戦略。
//
// scope 外:
//   - 実際の game state machine (= READY → playing → end)。WebSocket driven、
//     spec scope を超える。surrender / verify も同じ理由でスキップ。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('reversi/* multiplayer shape compat', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('reversi/games returns array shape (anonymous)', async ({ request }) => {
    const resp = await callApi(request, 'reversi/games', {});
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('reversi/show-game returns negative for unknown gameId', async ({ request }) => {
    // 不明 gameId は negative response (= 4xx)。
    //   - mk-go: 404 NO_SUCH_GAME
    //   - upstream Misskey TS: paramDef で format: 'misskey:id' を要求し、
    //     さらに Service.get 内で何らかの追加 validation があるため、
    //     well-formed alphanumeric でも 400 を返すケースあり。
    // どちらも frontend 側で「game not found」扱いの 4xx なので両方を許容。
    const resp = await callApi(request, 'reversi/show-game', {
      gameId: '9zzzzzzzzzzzzzzz',
    });
    expect([400, 404]).toContain(resp.status());
  });

  test('match → invitations → cancel-match round-trip', async ({ request }) => {
    const inviter = await signupUser(request, randomUsername('rvi'));
    const invitee = await signupUser(request, randomUsername('rvt'));

    // 1. inviter が invitee を指名して match。
    //    upstream は 204、mk-go は 200+body (#898 wontfix LCD)。
    const matchResp = await callApi(request, 'reversi/match', {
      i: inviter.token,
      userId: invitee.id,
    });
    expect([200, 204]).toContain(matchResp.status());

    // 2. invitee が invitations を引くと inviter が含まれる (= UserLite 配列)。
    const invResp = await callApi(request, 'reversi/invitations', {
      i: invitee.token,
    });
    expect(invResp.status()).toBe(200);
    const inviters = (await invResp.json()) as { id: string; username: string }[];
    expect(Array.isArray(inviters)).toBe(true);
    const found = inviters.find((u) => u.id === inviter.id);
    expect(found, 'invitee should see inviter in invitations').toBeDefined();
    expect(found?.username).toBe(inviter.username);

    // 3. inviter が cancel-match で自分の pending を取り下げ → 204。
    //    両 backend ともに 204 No Content を返すので strict 化。
    const cancelResp = await callApi(request, 'reversi/cancel-match', {
      i: inviter.token,
    });
    expect(cancelResp.status()).toBe(204);
    // cancel 後の invitations cleanup 挙動は backend 間で差あり (#899 wontfix):
    //   - mk-go: row 削除 + Redis cleanup で即時消える
    //   - TS:    Redis ZSET entry が残置されるので invitee 側からは依然として見える
    // どちらの shape も「機能的には正しい」ので strict-assert はしない (= no
    // further assertion below)。
  });
});
