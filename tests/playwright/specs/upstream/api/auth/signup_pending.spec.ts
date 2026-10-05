/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #809 / #810 Phase 2: signup-pending error response shape.
//
// upstream Misskey TS の `/api/signup-pending` は handler 全体を try-catch
// で囲み、catch 節で `throw new FastifyReplyError(400, ...)` を投げる
// (本家の SignupApiService.ts:285-287)。既知 error も unknown error
// も **status 400 + Fastify-style reply error shape** で返る:
//
//   {"statusCode":400,"error":"Bad Request","message":<string>}
//
// mk-go は #809 で同 status / shape に揃えた (旧 mk-go は NO_SUCH_CODE=404
// / EXPIRED=410 / INVITATION_ALREADY_USED=409 と独自 status を返していて
// drop-in 互換が崩れていた)。
//
// 注: message の **本文** は upstream と mk-go で異なる:
//   - upstream: typeORM の `EntityNotFoundError` を toString した生文字列
//     (= "EntityNotFoundError: Could not find any entity of type ...")
//   - mk-go: 安定 code "NO_SUCH_CODE" を含む Fastify shape
//     (= "Error: NO_SUCH_CODE")
// upstream は generic catch で漏れた未整理 error をそのまま返しているだけで
// frontend 側で意味のある classification ができないため、mk-go は安定 code を
// 出す方を選んだ (= shape 互換 + frontend 親和性 > 完全な message 一致)。
//
// 本 spec は両 backend で共通する status/shape (statusCode + error 文字列)
// と message が string であることだけ確認する。expired / invitation 系は
// spec 上で再現困難 (= 25h 前の pending row 操作 / invitation ticket DB
// 操作が必要) なので unit test 側で cover する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('auth: signup-pending error shape', () => {
  test.beforeEach(() => {
    resetRateLimit();
  });

  test('non-existent code returns 400 + Fastify-style reply error', async ({ request }) => {
    const resp = await callApi(request, 'signup-pending', { code: 'notexistcode' });
    expect(resp.status()).toBe(400);
    const body = await resp.json();
    expect(body.statusCode).toBe(400);
    expect(body.error).toBe('Bad Request');
    // message の本文は backend ごとに異なる (上記 file header 注を参照) ので
    // 本 spec では string 型と非空であることだけ確認する。
    expect(typeof body.message).toBe('string');
    expect(body.message.length).toBeGreaterThan(0);
  });
});
