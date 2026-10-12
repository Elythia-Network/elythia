/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #829 drive 拡張 PR-A: drive/files/update + drive/files/delete の正常系。
//
// upstream Misskey TS と mk-go (本 PR で fix 後) は両 endpoint で:
//   - update → 200 + `pack(file, { self: true })` shape (= self single,
//     folder / userId / user は null)
//   - delete → 204 NoContent
//
// 本 spec は両 backend 共通で:
//   1. 1x1 PNG upload → drive/files/update で name / isSensitive / comment
//      を変更 → response shape (self single) 確認 + drive/files/show で
//      永続化を確認
//   2. 別 user で 1x1 PNG upload → drive/files/delete → drive/files/show
//      で 4xx を確認 (= 削除確定)
//
// を検証する。drive/files/create の self single shape は #813 で別 spec、
// find/find-by-hash の self list shape は #842 で別 spec、本 spec は
// update / delete の round-trip + shape に focus。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { tinyPNG } from '../../../../fixtures/files';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

const baseURL = process.env.MK_BASE_URL ?? 'https://mkgo.local';

async function uploadTinyPNG(request: import('@playwright/test').APIRequestContext, token: string, name: string) {
  const resp = await request.post(`${baseURL}/api/drive/files/create`, {
    multipart: {
      i: token,
      file: { name, mimeType: 'image/png', buffer: tinyPNG },
    },
    failOnStatusCode: false,
  });
  if (resp.status() !== 200) {
    throw new Error(`upload failed: ${resp.status()} ${await resp.text()}`);
  }
  return resp.json();
}

test.describe('drive: files/update + files/delete', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('update modifies name / isSensitive / comment and reflects via files/show', async ({ request }) => {
    const me = await signupUser(request, randomUsername('drvU'));
    const uploaded = await uploadTinyPNG(request, me.token, 'before.png');

    const updateResp = await callApi(request, 'drive/files/update', {
      i: me.token,
      fileId: uploaded.id,
      name: 'after.png',
      comment: 'updated comment',
      isSensitive: true,
    });
    expect(updateResp.status()).toBe(200);
    const updated = await updateResp.json();
    expect(updated.id).toBe(uploaded.id);
    expect(updated.name).toBe('after.png');
    expect(updated.comment).toBe('updated comment');
    expect(updated.isSensitive).toBe(true);
    // self single shape (folder / userId / user は null) を upstream に揃える。
    expect(updated.folder).toBeNull();
    expect(updated.userId).toBeNull();
    expect(updated.user).toBeNull();

    // files/show で永続化を確認 (= shape は detail で userId / user が出る)。
    const showResp = await callApi(request, 'drive/files/show', {
      i: me.token,
      fileId: uploaded.id,
    });
    expect(showResp.status()).toBe(200);
    const got = await showResp.json();
    expect(got.name).toBe('after.png');
    expect(got.comment).toBe('updated comment');
    expect(got.isSensitive).toBe(true);
  });

  test('delete removes the file and subsequent show returns 4xx', async ({ request }) => {
    const me = await signupUser(request, randomUsername('drvD'));
    const uploaded = await uploadTinyPNG(request, me.token, 'gone.png');

    const delResp = await callApi(request, 'drive/files/delete', {
      i: me.token,
      fileId: uploaded.id,
    });
    expect(delResp.status()).toBe(204);

    // 削除確定確認: files/show で取得不可 (= 4xx)。
    //
    // upstream Misskey TS は files/delete が 204 を返した後、actual な
    // DB row 削除を async (= job queue) で行う形になっており、204 直後の
    // show が 200 を返す race がある (= TS image で 2-3/3 の頻度で発現)。
    // mk-go は同期 delete で常に 4xx を返す。
    //
    // 固定 sleep より expect.poll で polling する方が TS の async 削除
    // タイミング変動 (= job queue 負荷次第) にも自動追従し、必要最小の
    // 待ち時間で済む。status は mk-go=404 / TS=400 の drift があるが
    // 「4xx 範囲」で両 backend pass。
    await expect
      .poll(
        async () => {
          const resp = await callApi(request, 'drive/files/show', {
            i: me.token,
            fileId: uploaded.id,
          });
          const s = resp.status();
          return s >= 400 && s < 500;
        },
        { timeout: 5000, intervals: [100, 200, 500, 1000] },
      )
      .toBe(true);
  });
});
