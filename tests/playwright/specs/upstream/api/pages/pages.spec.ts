/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #825: pages CRUD round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - /api/pages/create で title + name を必須に取り、page を作成
//   - /api/pages/show で pageId 指定で取得 (== 同一 shape を返す)
//   - /api/pages/update で title 等を更新、show で反映
//   - /api/pages/delete で削除、show は 4xx
//   - /api/i/pages で自分の pages を list (= owner 自身の page が含まれる)
//
// Page の content / variables は jsonb の自由 schema なので spec では
// 空配列で固定し、shape の整合性 (id / userId / title / name) のみ検証する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('pages: CRUD round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('create / show / update / delete reflects via /api/i/pages', async ({ request }) => {
    const me = await signupUser(request, randomUsername('pgA'));
    const slug = `pg-${Date.now().toString(36)}`;

    // create
    const createResp = await callApi(request, 'pages/create', {
      i: me.token,
      title: 'My Page',
      name: slug,
      content: [],
      variables: [],
      script: '',
    });
    expect(createResp.status()).toBe(200);
    const created = await createResp.json();
    expect(typeof created.id).toBe('string');
    expect(created.userId).toBe(me.id);
    expect(created.title).toBe('My Page');
    expect(created.name).toBe(slug);

    // show by pageId
    const show1 = await callApi(request, 'pages/show', { i: me.token, pageId: created.id });
    expect(show1.status()).toBe(200);
    const shownByID = await show1.json();
    expect(shownByID.id).toBe(created.id);
    expect(shownByID.title).toBe('My Page');

    // update
    const updateResp = await callApi(request, 'pages/update', {
      i: me.token,
      pageId: created.id,
      title: 'Updated Page',
      name: slug,
      content: [],
      variables: [],
      script: '',
    });
    // update は upstream TS では 204、mk-go では 200 + page を返す。
    // どちらでも 2xx で OK と扱い、続けて show で更新反映を検証する。
    expect([200, 204]).toContain(updateResp.status());

    const show2 = await callApi(request, 'pages/show', { i: me.token, pageId: created.id });
    expect(show2.status()).toBe(200);
    const updated = await show2.json();
    expect(updated.title).toBe('Updated Page');

    // /api/i/pages で自分の page が含まれる
    const myList = await callApi(request, 'i/pages', { i: me.token });
    expect(myList.status()).toBe(200);
    const myPages = await myList.json();
    expect(Array.isArray(myPages)).toBe(true);
    expect(myPages.find((p: { id: string }) => p.id === created.id)).toBeTruthy();

    // delete
    const deleteResp = await callApi(request, 'pages/delete', {
      i: me.token,
      pageId: created.id,
    });
    expect([200, 204]).toContain(deleteResp.status());

    // delete 後の show は 4xx (NO_SUCH_PAGE)
    const show3 = await callApi(request, 'pages/show', { i: me.token, pageId: created.id });
    expect(show3.status()).toBeGreaterThanOrEqual(400);
    expect(show3.status()).toBeLessThan(500);
  });
});
