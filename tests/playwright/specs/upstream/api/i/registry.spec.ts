/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #836: i/registry/* round-trip (= K-V 永続化)。
//
// upstream Misskey TS と mk-go は両方とも generic な K-V store を提供:
//   - i/registry/set { key, value, scope[] } で値を保存 (204)
//   - i/registry/get { key, scope[] } で raw JSON を取得 (= JSONBlob)
//   - i/registry/get-all { scope[] } で同 scope 全 key を { key: value } map で取得
//   - i/registry/keys-with-type { scope[] } で key の type 一覧
//   - i/registry/remove { key, scope[] } で削除 (204)
//
// scope は Misskey frontend が UI 設定を namespace 単位で持つための concept
// (= ['client', 'preferences'] のような string array)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('i/registry K-V CRUD round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('set / get / get-all / keys-with-type / remove round-trip', async ({ request }) => {
    const me = await signupUser(request, randomUsername('regA'));
    const scope = ['spec', 'phase3'];
    const key1 = `key1_${Math.random().toString(16).slice(2, 8)}`;
    const key2 = `key2_${Math.random().toString(16).slice(2, 8)}`;

    // set key1 → object value
    const set1Resp = await callApi(request, 'i/registry/set', {
      i: me.token,
      key: key1,
      value: { nested: { foo: 'bar' }, n: 42 },
      scope,
    });
    expect([200, 204]).toContain(set1Resp.status());

    // set key2 → primitive (string)
    const set2Resp = await callApi(request, 'i/registry/set', {
      i: me.token,
      key: key2,
      value: 'hello',
      scope,
    });
    expect([200, 204]).toContain(set2Resp.status());

    // get key1 → JSONBlob で同 object が返る
    const getResp = await callApi(request, 'i/registry/get', {
      i: me.token,
      key: key1,
      scope,
    });
    expect(getResp.status()).toBe(200);
    const got = (await getResp.json()) as { nested: { foo: string }; n: number };
    expect(got.nested.foo).toBe('bar');
    expect(got.n).toBe(42);

    // get-all → 同 scope 内 key1 + key2 が含まれる
    const getAllResp = await callApi(request, 'i/registry/get-all', {
      i: me.token,
      scope,
    });
    expect(getAllResp.status()).toBe(200);
    const all = (await getAllResp.json()) as Record<string, unknown>;
    expect(all[key1]).toBeDefined();
    expect(all[key2]).toBe('hello');

    // keys-with-type → key1 / key2 が含まれる (= type information は backend
    // 実装で異なるので key 存在のみ assert)
    const keysResp = await callApi(request, 'i/registry/keys-with-type', {
      i: me.token,
      scope,
    });
    expect(keysResp.status()).toBe(200);
    const keys = (await keysResp.json()) as Record<string, unknown>;
    expect(keys[key1]).toBeDefined();
    expect(keys[key2]).toBeDefined();

    // remove key1 → 以降 get で 4xx
    const removeResp = await callApi(request, 'i/registry/remove', {
      i: me.token,
      key: key1,
      scope,
    });
    expect([200, 204]).toContain(removeResp.status());

    const getAfterRemove = await callApi(request, 'i/registry/get', {
      i: me.token,
      key: key1,
      scope,
    });
    expect(getAfterRemove.status()).toBeGreaterThanOrEqual(400);
    expect(getAfterRemove.status()).toBeLessThan(500);

    // get-all で key1 が消えて key2 のみ残る (cleanup verification)
    const getAllAfter = await callApi(request, 'i/registry/get-all', {
      i: me.token,
      scope,
    });
    expect(getAllAfter.status()).toBe(200);
    const allAfter = (await getAllAfter.json()) as Record<string, unknown>;
    expect(allAfter[key1]).toBeUndefined();
    expect(allAfter[key2]).toBe('hello');

    // cleanup: key2 も消す (test 後の orphan 防止)
    await callApi(request, 'i/registry/remove', {
      i: me.token,
      key: key2,
      scope,
    });
  });

  // domain field の挙動には backend drift があり別 issue で追跡する (#907):
  //   - mk-go: regular user token でも `domain: <free-string>` を accept、
  //     domain による namespace isolation が機能
  //   - upstream TS: regular user token + free-string domain では 400 を返す
  //     (= app token (accessToken) 経由でないと domain を指定できない)
  // 本 spec scope では domain 経路を直接 verify しない LCD に留める。
});
