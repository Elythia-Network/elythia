/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #829 drive 拡張 PR-B: drive/folders CRUD + nest 表示。
//
// upstream Misskey TS と mk-go (#845 fix 後) は drive/folders の各 endpoint
// で以下の shape を返す:
//   - /api/drive/folders/create / update / find: default mode
//     `{ id, createdAt, name, parentId }`
//   - /api/drive/folders/show: detail mode = default 4 field +
//     `{ foldersCount, filesCount, parent? }` (両 backend で揃う、#845 fix 済)
//   - /api/drive/folders/delete: 204 NoContent
//
// 検証する round-trip:
//   1. folder create → 4 field shape assert
//   2. show → 同 shape (= 永続化確認)
//   3. update (rename) → name 反映 + show で再確認
//   4. delete → 204 + show が 4xx で削除確定
//   5. nest: parent folder 内に child folder を作り parentId が一致

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('drive: folders CRUD', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('create / show / update / delete round-trip', async ({ request }) => {
    const me = await signupUser(request, randomUsername('drvFc'));

    // create
    const createResp = await callApi(request, 'drive/folders/create', {
      i: me.token,
      name: 'CRUD-folder',
    });
    expect(createResp.status()).toBe(200);
    const created = await createResp.json();
    expect(typeof created.id).toBe('string');
    expect(created.name).toBe('CRUD-folder');
    expect(created.parentId).toBeNull();
    expect(typeof created.createdAt).toBe('string');

    // show: default 4 field + detail field (foldersCount/filesCount/parent)
    // を strict assert (#845 で両 backend で揃った)。
    const showResp = await callApi(request, 'drive/folders/show', {
      i: me.token,
      folderId: created.id,
    });
    expect(showResp.status()).toBe(200);
    const got = await showResp.json();
    expect(got.id).toBe(created.id);
    expect(got.name).toBe('CRUD-folder');
    expect(got.parentId).toBeNull();
    // detail field (子なし state なので 0、parentId=nil なので parent なし)。
    expect(got.foldersCount).toBe(0);
    expect(got.filesCount).toBe(0);
    expect(got.parent).toBeUndefined();

    // update: rename
    const updateResp = await callApi(request, 'drive/folders/update', {
      i: me.token,
      folderId: created.id,
      name: 'CRUD-folder-renamed',
    });
    expect(updateResp.status()).toBe(200);
    const updated = await updateResp.json();
    expect(updated.id).toBe(created.id);
    expect(updated.name).toBe('CRUD-folder-renamed');

    // delete + show が 4xx で削除確定
    const delResp = await callApi(request, 'drive/folders/delete', {
      i: me.token,
      folderId: created.id,
    });
    expect(delResp.status()).toBe(204);

    // delete も async DB 削除の race を考慮、polling で 4xx を待つ
    // (drive/files/delete spec #843 と同じ pattern)。
    await expect
      .poll(
        async () => {
          const resp = await callApi(request, 'drive/folders/show', {
            i: me.token,
            folderId: created.id,
          });
          const s = resp.status();
          return s >= 400 && s < 500;
        },
        { timeout: 5000, intervals: [100, 200, 500, 1000] },
      )
      .toBe(true);
  });

  test('nested folder reflects parentId', async ({ request }) => {
    const me = await signupUser(request, randomUsername('drvFn'));

    // 親 folder
    const parentResp = await callApi(request, 'drive/folders/create', {
      i: me.token,
      name: 'parent',
    });
    expect(parentResp.status()).toBe(200);
    const parent = await parentResp.json();
    expect(parent.parentId).toBeNull();

    // 子 folder (parentId 指定)
    const childResp = await callApi(request, 'drive/folders/create', {
      i: me.token,
      name: 'child',
      parentId: parent.id,
    });
    expect(childResp.status()).toBe(200);
    const child = await childResp.json();
    expect(child.parentId).toBe(parent.id);

    // show でも parentId が一致 + detail field の parent (recursive pack)
    // が親 folder を埋める (#845)。
    const showResp = await callApi(request, 'drive/folders/show', {
      i: me.token,
      folderId: child.id,
    });
    expect(showResp.status()).toBe(200);
    const got = await showResp.json();
    expect(got.parentId).toBe(parent.id);
    expect(got.foldersCount).toBe(0);
    expect(got.filesCount).toBe(0);
    // parent recursive detail pack: 親 folder の id / foldersCount=1 (= 子
    // を 1 個持つ) を確認。
    expect(got.parent).toBeDefined();
    expect(got.parent.id).toBe(parent.id);
    expect(got.parent.foldersCount).toBe(1);
    expect(got.parent.filesCount).toBe(0);
  });
});
