/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #833: antennas CRUD round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - antennas/create で antenna 設定 (name, src, keywords, etc) を送る
//   - antennas/show で antennaId 指定で取得
//   - antennas/list で自分の antenna 一覧
//   - antennas/update で部分更新 (name / keywords / etc)
//   - antennas/delete で削除
//
// antennas/notes (= timeline 経路) は #819 で別 cover 済。本 spec は CRUD
// 部分に集中する。
//
// keywords は [[group_a_kw1, group_a_kw2], [group_b_kw1]] の形で AND-of-OR
// 構造。spec では deterministic な単純 keyword で round-trip のみ確認する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

// Antenna の packed shape (upstream Misskey TS と mk-go #904 fix 後で
// 一致)。userId field は両 backend で **含まない** (= antenna は user-scoped、
// frontend が呼出主の id を別経路で持つ設計)。
interface Antenna {
  id: string;
  name: string;
  src: string;
  keywords?: string[][];
  excludeKeywords?: string[][];
  caseSensitive?: boolean;
  withReplies?: boolean;
  withFile?: boolean;
  localOnly?: boolean;
  isActive?: boolean;
}

test.describe('antennas: CRUD round-trip', () => {
  // assertion 失敗時に DB に orphan antenna が残らないよう、test 単位で
  // afterEach cleanup する。正規 path で delete 済の場合は idempotent に
  // 4xx を許容する (= 削除済 antenna への delete は NO_SUCH_ANTENNA)。
  let createdAntennaId: string | undefined;
  let userToken: string | undefined;

  test.beforeAll(() => {
    resetRateLimit();
  });

  test.afterEach(async ({ request }) => {
    if (createdAntennaId && userToken) {
      await callApi(request, 'antennas/delete', {
        i: userToken,
        antennaId: createdAntennaId,
      });
    }
    createdAntennaId = undefined;
    userToken = undefined;
  });

  test('create / show / list / update / delete round-trip', async ({ request }) => {
    const me = await signupUser(request, randomUsername('antA'));
    userToken = me.token;
    const name = `antenna_${Math.random().toString(16).slice(2, 8)}`;

    // create
    const createResp = await callApi(request, 'antennas/create', {
      i: me.token,
      name,
      src: 'all',
      keywords: [['hello']],
      excludeKeywords: [],
      users: [],
      caseSensitive: false,
      withReplies: false,
      withFile: false,
      localOnly: false,
    });
    expect(createResp.status()).toBe(200);
    // packed antenna は upstream Misskey TS / mk-go (#904 fix 後) いずれも
    // userId を含まない。両 backend の shape を strict に揃えた regression
    // guard として userId 不在を直接 assert する。
    const created = (await createResp.json()) as Record<string, unknown>;
    expect(typeof created.id).toBe('string');
    createdAntennaId = created.id as string;
    expect(created.userId).toBeUndefined();
    expect(created.name).toBe(name);
    expect(created.src).toBe('all');
    expect(created.keywords).toEqual([['hello']]);

    // show
    const showResp = await callApi(request, 'antennas/show', {
      i: me.token,
      antennaId: created.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = (await showResp.json()) as Antenna;
    expect(shown.id).toBe(created.id);
    expect(shown.name).toBe(name);

    // list で自分の antenna が含まれる
    const listResp = await callApi(request, 'antennas/list', { i: me.token });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as Antenna[];
    expect(Array.isArray(list)).toBe(true);
    expect(list.find((a) => a.id === created.id)).toBeTruthy();

    // update — name + keywords を変更、空 keywords / users で同 drift 経路も touch
    const newName = `${name}_updated`;
    const updateResp = await callApi(request, 'antennas/update', {
      i: me.token,
      antennaId: created.id,
      name: newName,
      keywords: [['hi', 'world']],
      // empty users で antenna_service.go::Update の pq.StringArray wrap path
      // を実行 (#896 の同 anti-pattern を fix 済 = NOT NULL 違反にならない)。
      users: [],
    });
    expect([200, 204]).toContain(updateResp.status());

    const showAfterUpdate = await callApi(request, 'antennas/show', {
      i: me.token,
      antennaId: created.id,
    });
    expect(showAfterUpdate.status()).toBe(200);
    const updated = (await showAfterUpdate.json()) as Antenna;
    expect(updated.name).toBe(newName);
    expect(updated.keywords).toEqual([['hi', 'world']]);

    // delete
    const deleteResp = await callApi(request, 'antennas/delete', {
      i: me.token,
      antennaId: created.id,
    });
    expect([200, 204]).toContain(deleteResp.status());
    // 正規 path で delete 済 = afterEach cleanup の必要なし
    createdAntennaId = undefined;

    // delete 後の show は 4xx (NO_SUCH_ANTENNA)
    const showAfterDelete = await callApi(request, 'antennas/show', {
      i: me.token,
      antennaId: created.id,
    });
    expect(showAfterDelete.status()).toBeGreaterThanOrEqual(400);
    expect(showAfterDelete.status()).toBeLessThan(500);

    // list からも消える
    const listAfterDelete = await callApi(request, 'antennas/list', { i: me.token });
    expect(listAfterDelete.status()).toBe(200);
    const listAfter = (await listAfterDelete.json()) as Antenna[];
    expect(listAfter.find((a) => a.id === created.id)).toBeFalsy();
  });
});
