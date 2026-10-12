/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #825: flash (AiScript Play) CRUD round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - /api/flash/create で title + script を必須に取り、flash を作成
//   - /api/flash/show で flashId 指定で取得
//   - /api/flash/update で title / script / summary / permissions を更新
//   - /api/flash/delete で削除
//   - /api/flash/my で自分の flash 一覧
//
// permissions は AiScript runtime の granted scope だが spec では空配列で
// 固定し shape compat のみ検証する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('flash: CRUD round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('create / show / update / delete reflects via /api/flash/my', async ({ request }) => {
    const me = await signupUser(request, randomUsername('flA'));

    // create
    const createResp = await callApi(request, 'flash/create', {
      i: me.token,
      title: 'Hello Play',
      summary: 'demo',
      script: '<:: "hello">',
      permissions: [],
      visibility: 'public',
    });
    expect(createResp.status()).toBe(200);
    const created = await createResp.json();
    expect(typeof created.id).toBe('string');
    expect(created.userId).toBe(me.id);
    expect(created.title).toBe('Hello Play');
    expect(created.script).toBe('<:: "hello">');

    // show
    const showResp = await callApi(request, 'flash/show', {
      i: me.token,
      flashId: created.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.id).toBe(created.id);
    expect(shown.title).toBe('Hello Play');

    // /api/flash/my で自分の flash が含まれる
    const myResp = await callApi(request, 'flash/my', { i: me.token });
    expect(myResp.status()).toBe(200);
    const myList = await myResp.json();
    expect(Array.isArray(myList)).toBe(true);
    expect(myList.find((f: { id: string }) => f.id === created.id)).toBeTruthy();

    // update — permissions に空配列 [] を渡しても NOT NULL 制約に当たらず
    // 200 で更新できる (#896 で fix)。pq.StringArray wrap の regression
    // guard として frontend と同じ payload を送る。
    const updateResp = await callApi(request, 'flash/update', {
      i: me.token,
      flashId: created.id,
      title: 'Updated Play',
      summary: 'updated summary',
      script: '<:: "updated">',
      permissions: [],
      visibility: 'public',
    });
    expect([200, 204]).toContain(updateResp.status());

    const showAfterUpdate = await callApi(request, 'flash/show', {
      i: me.token,
      flashId: created.id,
    });
    expect(showAfterUpdate.status()).toBe(200);
    const updated = await showAfterUpdate.json();
    expect(updated.title).toBe('Updated Play');
    expect(updated.script).toBe('<:: "updated">');

    // delete
    const deleteResp = await callApi(request, 'flash/delete', {
      i: me.token,
      flashId: created.id,
    });
    expect([200, 204]).toContain(deleteResp.status());

    // delete 後の show は 4xx
    const showAfterDelete = await callApi(request, 'flash/show', {
      i: me.token,
      flashId: created.id,
    });
    expect(showAfterDelete.status()).toBeGreaterThanOrEqual(400);
    expect(showAfterDelete.status()).toBeLessThan(500);
  });
});
