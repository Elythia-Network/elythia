/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #837 PR-A: bubble-game/* round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - bubble-game/register: { score, seed, logs, gameMode, gameVersion } で
//     ハイスコア record を作成 → 204 No Content (return value なし)。
//   - bubble-game/ranking: { gameMode } で上位 10 件を返す → array of
//     { id, score, user: { id, username, ... } }
//
// 本 spec は:
//   1. 仮想 score で register → 204
//   2. 直後に ranking を叩いて自分が含まれていること (= 反映確認)
//   3. ranking shape (= id / score / user.id 等)
// を verify する LCD strategy。
//
// upstream の rate limit (30sec minInterval, 120/hour) は spec 1 回叩きで
// 影響なし。

import { randomUUID } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RankingEntry {
  id: string;
  score: number;
  user?: { id: string; username: string };
}

test.describe('bubble-game/* register + ranking round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('register score then appear in ranking', async ({ request }) => {
    const me = await signupUser(request, randomUsername('bbg'));
    // 高 score を入れて確実に top 10 に入る (= ranking 内蔵 limit 10)。
    // seed は upstream / mk-go ともに「Unix ms epoch を文字列化したもの」を
    // 期待し、5 時間以内である必要がある。直近の Date.now() を文字列化。
    const seed = String(Date.now());
    // gameMode を unique 文字列にして他 record と分離。worker 並列化時の
    // 衝突を避けるため UUID v4 を使う (= miauth / sw spec と同 pattern)。
    const gameMode = `spec_${randomUUID()}`;
    const score = 999_999_999;

    const regResp = await callApi(request, 'bubble-game/register', {
      i: me.token,
      score,
      seed,
      logs: [],
      gameMode,
      gameVersion: 1,
    });
    // upstream / mk-go ともに 204 No Content (= return value なし)。
    expect(regResp.status()).toBe(204);

    const rankResp = await callApi(request, 'bubble-game/ranking', { gameMode });
    expect(rankResp.status()).toBe(200);
    const ranking = (await rankResp.json()) as RankingEntry[];
    expect(Array.isArray(ranking)).toBe(true);
    // gameMode が unique 文字列なので登録した自分のみ含まれる。
    expect(ranking.length).toBe(1);
    const entry = ranking[0];
    expect(typeof entry.id).toBe('string');
    expect(entry.score).toBe(score);
    expect(entry.user?.id).toBe(me.id);
  });

  test('ranking with unknown gameMode returns empty array', async ({ request }) => {
    // anonymous 可 (= requireCredential: false)。未登録 gameMode は空配列。
    const resp = await callApi(request, 'bubble-game/ranking', {
      gameMode: `nope_${randomUUID()}`,
    });
    expect(resp.status()).toBe(200);
    expect(await resp.json()).toEqual([]);
  });
});
