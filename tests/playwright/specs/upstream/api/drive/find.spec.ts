/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #829 drive 拡張 PR-C: drive/files/find と drive/files/find-by-hash の
// 正常系。
//
// upstream Misskey TS と mk-go (#841 で fix 後) は両 endpoint で
// `packMany(files, { self: true })` 経路 = `packNullable` shape を返す:
//   - folder: null (= detail=false)
//   - user: null (= withUser=false)
//   - userId: owner ID 維持 (= packNullable は userId を常時返す)
//
// 本 spec は両 backend 共通で:
//   1. 1x1 transparent PNG を upload (= drive/files/create)
//   2. find-by-hash で md5 検索 → shape strict assert
//   3. find で name 検索 → shape strict assert
//
// を検証する。drive/files/create の self single shape は別 spec
// (drive/create.spec.ts) で cover、本 spec は self list path に focus。
//
// #818 で発見した self path drift を unit test に加えて Playwright
// e2e でも担保 = drift 再導入を browser 経由でも catch できる。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { tinyPNG } from '../../../../fixtures/files';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

const baseURL = process.env.MK_BASE_URL ?? 'https://mkgo.local';

test.describe('drive: files/find + find-by-hash', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('find-by-hash returns the uploaded file with self list shape', async ({ request }) => {
    const me = await signupUser(request, randomUsername('drvF'));

    const fileName = `find-bh-${Math.random().toString(16).slice(2, 8)}.png`;
    const uploadResp = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: {
        i: me.token,
        file: { name: fileName, mimeType: 'image/png', buffer: tinyPNG },
      },
      failOnStatusCode: false,
    });
    expect(uploadResp.status()).toBe(200);
    const uploaded = await uploadResp.json();
    expect(uploaded.md5).toBeTruthy();

    const findResp = await callApi(request, 'drive/files/find-by-hash', {
      i: me.token,
      md5: uploaded.md5,
    });
    expect(findResp.status()).toBe(200);
    const list = await findResp.json();
    expect(Array.isArray(list)).toBe(true);
    expect(list.length).toBeGreaterThanOrEqual(1);

    // upload した file が含まれる。upload と find で id 一致を確認。
    const hit = list.find((f: { id: string }) => f.id === uploaded.id);
    expect(hit).toBeDefined();

    // self list shape: folder/user は null、userId は owner ID 維持 (#818)。
    expect(hit.folder).toBeNull();
    expect(hit.user).toBeNull();
    expect(hit.userId).toBe(me.id);
  });

  test('find by name returns the uploaded file with self list shape', async ({ request }) => {
    const me = await signupUser(request, randomUsername('drvFn'));

    const fileName = `find-name-${Math.random().toString(16).slice(2, 8)}.png`;
    const uploadResp = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: {
        i: me.token,
        file: { name: fileName, mimeType: 'image/png', buffer: tinyPNG },
      },
      failOnStatusCode: false,
    });
    expect(uploadResp.status()).toBe(200);
    const uploaded = await uploadResp.json();

    const findResp = await callApi(request, 'drive/files/find', {
      i: me.token,
      name: fileName,
    });
    expect(findResp.status()).toBe(200);
    const list = await findResp.json();
    expect(Array.isArray(list)).toBe(true);
    expect(list.length).toBe(1);

    const hit = list[0];
    expect(hit.id).toBe(uploaded.id);
    expect(hit.name).toBe(fileName);

    // self list shape: folder/user は null、userId は owner ID 維持 (#818)。
    expect(hit.folder).toBeNull();
    expect(hit.user).toBeNull();
    expect(hit.userId).toBe(me.id);
  });
});
