/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #825: gallery CRUD round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - /api/gallery/posts/create で title + fileIds でポストを作成
//   - /api/gallery/posts/show で postId 指定で取得
//   - /api/gallery/posts/update で title / description を更新
//   - /api/gallery/posts/delete で削除
//   - /api/gallery/posts で post 一覧 (公開分)
//
// upstream TS は paramDef で fileIds を min:1 で要求するので、本 spec は
// 1x1 PNG を drive/files/create で upload した上で fileIds=[id] を渡す。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { tinyPNG } from '../../../../fixtures/files';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

const baseURL = process.env.MK_BASE_URL ?? 'https://mkgo.local';

test.describe('gallery: posts CRUD round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('create / show / update / delete round-trip', async ({ request }) => {
    const me = await signupUser(request, randomUsername('glA'));

    // tiny PNG を upload して fileId を取得する (TS は paramDef で fileIds
    // min:1 を要求、mk-go は []  も accept する drift があるが、両 backend
    // で動く LCD として実 file を渡す pattern にする)。
    const uploadResp = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: {
        i: me.token,
        file: { name: 'tiny.png', mimeType: 'image/png', buffer: tinyPNG },
      },
      failOnStatusCode: false,
    });
    expect(uploadResp.status()).toBe(200);
    const uploaded = await uploadResp.json();
    expect(typeof uploaded.id).toBe('string');

    // create
    const createResp = await callApi(request, 'gallery/posts/create', {
      i: me.token,
      title: 'My Gallery',
      description: 'desc',
      fileIds: [uploaded.id],
      isSensitive: false,
    });
    expect(createResp.status()).toBe(200);
    const created = await createResp.json();
    expect(typeof created.id).toBe('string');
    expect(created.title).toBe('My Gallery');
    expect(created.description).toBe('desc');
    // userId は upstream で string、author user object は user field に入る
    expect(typeof created.userId).toBe('string');

    // show by postId
    const showResp = await callApi(request, 'gallery/posts/show', {
      i: me.token,
      postId: created.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.id).toBe(created.id);
    expect(shown.title).toBe('My Gallery');

    // update title + description (fileIds は変更しないが TS の paramDef は
    // min:1 を要求するので uploaded.id を再度渡す)
    const updateResp = await callApi(request, 'gallery/posts/update', {
      i: me.token,
      postId: created.id,
      title: 'Updated Gallery',
      description: 'new desc',
      fileIds: [uploaded.id],
    });
    expect([200, 204]).toContain(updateResp.status());

    const showAfterUpdate = await callApi(request, 'gallery/posts/show', {
      i: me.token,
      postId: created.id,
    });
    expect(showAfterUpdate.status()).toBe(200);
    const updated = await showAfterUpdate.json();
    expect(updated.title).toBe('Updated Gallery');
    expect(updated.description).toBe('new desc');

    // delete
    const deleteResp = await callApi(request, 'gallery/posts/delete', {
      i: me.token,
      postId: created.id,
    });
    expect([200, 204]).toContain(deleteResp.status());

    // delete 後の show は 4xx (NO_SUCH_POST)
    const showAfterDelete = await callApi(request, 'gallery/posts/show', {
      i: me.token,
      postId: created.id,
    });
    expect(showAfterDelete.status()).toBeGreaterThanOrEqual(400);
    expect(showAfterDelete.status()).toBeLessThan(500);
  });
});
