/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1 残: drive/files/create の正常系。
//
// upstream Misskey TS と mk-go の `/api/drive/files/create` は
// multipart/form-data で file を受け取り、drive_file row を作って
// packed shape を返す。本 spec は両 backend 共通で:
//
//   1. 1x1 transparent PNG を multipart upload
//   2. response が 200 + 期待 shape (id/name/type/url/size)
//   3. drive/files/show で同 file を取得して整合性確認
//
// を assert する。
//
// upstream は `pack(file, { self: true })` (= withUser=false) で userId
// と user を null にする。mk-go も #812 で同 shape に揃え済 = 本 spec で
// strict assert する。
//
// Phase 1 残 spec の中で最も isolation が clean (= signup 経路で fresh
// user / 他 spec への副作用なし) なため最初に入れる。streaming /
// 2FA-passkey は別 spec で個別に整備する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { tinyPNG } from '../../../../fixtures/files';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

const baseURL = process.env.MK_BASE_URL ?? 'https://mkgo.local';

test.describe('drive: files/create', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('uploads a tiny PNG and exposes it via files/show', async ({ request }) => {
    const me = await signupUser(request, randomUsername('drv'));

    const uploadResp = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: {
        i: me.token,
        file: {
          name: 'tiny.png',
          mimeType: 'image/png',
          buffer: tinyPNG,
        },
      },
      failOnStatusCode: false,
    });
    expect(uploadResp.status()).toBe(200);
    const file = await uploadResp.json();
    expect(file.id).toBeTruthy();
    expect(file.name).toBe('tiny.png');
    expect(file.type).toBe('image/png');
    // url は scheme + host + `/files/` path prefix を strict に assert。
    // upstream / mk-go (router.go:1392 で `/files/:accessKey` を提供) どちら
    // も `/files/<accessKey>` を返すので、URL drift (= path-only / 別 endpoint
    // / scheme 欠落) を catch できる。scheme は backend config 依存で http/https
    // どちらも valid。
    expect(file.url).toMatch(/^https?:\/\/[^/]+\/files\//);
    expect(file.size).toBe(tinyPNG.length);
    // upstream `pack(file, { self: true })` (= withUser=false / detail=false)
    // 経路に整合。folder / userId / user は全て null。mk-go は #812 で
    // userId/user の handler 修正 + entity DriveFile の omitempty 解除で
    // 同 shape に揃えている。
    expect(file.folder).toBeNull();
    expect(file.userId).toBeNull();
    expect(file.user).toBeNull();

    // files/show で同 file を取得して shape 整合を確認。create と show で
    // identifier 系 (id) だけでなく metadata (name/type/size) も一致する
    // ことを assert することで、内部 state が正しく persist されたことを
    // strict に担保する。
    const showResp = await callApi(request, 'drive/files/show', {
      i: me.token,
      fileId: file.id,
    });
    expect(showResp.status()).toBe(200);
    const got = await showResp.json();
    expect(got.id).toBe(file.id);
    expect(got.name).toBe(file.name);
    expect(got.type).toBe(file.type);
    expect(got.size).toBe(file.size);
  });
});
