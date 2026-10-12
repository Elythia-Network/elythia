/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #825: reversi 招待 + match round-trip。
//
// Reversi の e2e (実対局) は scope 大なので、ここでは CRUD 系の挙動のみ:
//   - /api/reversi/match で targetUser=B 指定でゲーム作成 (= B への招待)
//   - /api/reversi/invitations (B 視点) で A の招待が見える
//   - /api/reversi/cancel-match (A 視点) で pending 招待を取り消し
//
// 完全な対局 (ready / put / 終局) は別 Phase で扱う。本 spec は match
// セッションの create / read / cancel に絞り両 backend で同 semantics で
// 動作することを担保する。upstream TS の match は 204 (body なし) を返し、
// mk-go は 200 + game body を返す drift があるので、game.id を直接取らず
// invitations 経由で round-trip を検証する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('reversi: match invitation + cancel round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('match creates pending invite, B sees it, A cancels', async ({ request }) => {
    const A = await signupUser(request, randomUsername('rvA'));
    const B = await signupUser(request, randomUsername('rvB'));

    // A が B に対戦招待を送る。match の return shape は backend で drift:
    //   - upstream Misskey TS: 204 No Content (= invitation 投函のみ)
    //   - mk-go: 200 + game body (= 即座に game row を返す)
    // 両 backend で動く LCD として 200 / 204 両許容、body 検証は省く (#898)。
    // game row の存在は B 視点 invitations + show-game で別途確認する。
    const matchResp = await callApi(request, 'reversi/match', {
      i: A.token,
      userId: B.id,
    });
    expect([200, 204]).toContain(matchResp.status());

    // B 視点 invitations に A が含まれる (= UserLite list)。upstream TS
    // の invitations は inviter UserLite[] を返す本家互換 shape。mk-go も
    // 同 shape で揃えてある。
    const invitations = await callApi(request, 'reversi/invitations', { i: B.token });
    expect(invitations.status()).toBe(200);
    const inviters = await invitations.json();
    expect(Array.isArray(inviters)).toBe(true);
    expect(inviters.find((u: { id: string }) => u.id === A.id)).toBeTruthy();

    // A が cancel-match で pending 招待を取り消す。両 backend で 2xx を返す
    // ことを担保する。cancel 後の invitations 側の cleanup tail は backend で
    // drift (TS は Redis pending invitation entry を残し、mk-go は game row
    // 削除と同時に list からも消える、#899 で追跡) があるので、ここでは
    // cancel API の return shape のみ検証する。
    const cancelResp = await callApi(request, 'reversi/cancel-match', { i: A.token });
    expect([200, 204]).toContain(cancelResp.status());
  });
});
