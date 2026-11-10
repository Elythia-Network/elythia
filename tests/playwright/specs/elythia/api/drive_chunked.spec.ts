/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 分割アップロード (#2313) の 4 つの口を 2xx まで通し、応答を /api.json の宣言で照合する
// (#4093)。分割アップロードは S3 の multipart upload で組み立てるので、ローカルの保存先では
// CHUNKED_UPLOAD_UNAVAILABLE で断られる。テストの間だけ、保存先を compose.yml の minio に
// 向け、分割アップロードを有効にする。終わったら、オブジェクトストレージを必ず無効に戻す
// (残すと、以後のドライブのアップロードの spec がすべて minio に書く)。

import { createHash, randomBytes } from 'node:crypto';
import { expect, test, type APIRequestContext } from '@playwright/test';
import { baseURL, callApi } from '../../../fixtures/api';
import { callChecked, expectApiResponse } from '../../../fixtures/api_schema';
import { rootFixture } from '../../../fixtures/admin_table';
import { randomUsername, signupUser, type Principal } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';

// チャンクの大きさの下限 (internal/core/drive/chunked_upload.go の MinChunkSizeMb)。S3 の
// multipart upload は最後以外の部分に 5MiB 以上を要求するので、これより小さくはできない
const CHUNK_MB = 5;
const CHUNK = CHUNK_MB * 1024 * 1024;

// compose.yml の minio / minio-init と揃える
const STORAGE_META = {
  useObjectStorage: true,
  objectStorageBucket: 'drive',
  objectStoragePrefix: 'pw-chunked',
  // ファイルの url はこのホストから作られる。ドットの無い minio は、照合の url 書式で落ちる
  objectStorageEndpoint: 'minio.local',
  objectStoragePort: 9000,
  objectStorageRegion: 'us-east-1',
  objectStorageAccessKey: 'minioadmin',
  objectStorageSecretKey: 'minioadmin-secret',
  objectStorageUseSSL: false,
  objectStorageS3ForcePathStyle: true,
  objectStorageSetPublicRead: false,
  chunkedUploadEnabled: true,
  chunkedUploadChunkSizeMb: CHUNK_MB,
};

interface Session { uploadId: string; chunkSize: number; totalChunks: number; expiresAt: string }
interface Appended { index: number; next: number; receivedBytes: number; totalSize: number; completed: boolean }

/** Starts a chunked upload of `size` bytes and checks the response. */
async function start(request: APIRequestContext, user: Principal, name: string, size: number): Promise<Session> {
  // expiresAt は time.Now() から作るので、いつも Go の書式 (ナノ秒まで) で返る。#4091 で直す
  return callChecked<Session>(request, 'drive/files/create-chunked/start', { i: user.token, name, size }, { knownBug: '#4091' });
}

/** Appends the chunk `index` of `data` as multipart and checks the response. */
async function append(request: APIRequestContext, user: Principal, uploadId: string, data: Buffer, index: number): Promise<Appended> {
  const resp = await request.post(`${baseURL()}/api/drive/files/create-chunked/append`, {
    multipart: {
      i: user.token,
      uploadId,
      index: String(index),
      chunk: { name: 'blob', mimeType: 'text/plain', buffer: data.subarray(index * CHUNK, (index + 1) * CHUNK) },
    },
    failOnStatusCode: false,
  });
  return expectApiResponse<Appended>(request, 'drive/files/create-chunked/append', resp);
}

test.describe('drive: the chunked upload through an S3 compatible storage', () => {
  test.skip(isTsBackend, '分割アップロードは Elythia 独自');
  test.setTimeout(90_000);

  // 片付けは afterAll で、新しい context から送る。test の中の finally は、時間切れで落ちると
  // request の fixture が先に破棄されて送れず、オブジェクトストレージが有効のまま残る
  let original: Record<string, unknown> | undefined;
  let roleId: string | undefined;

  test.beforeAll(() => {
    resetRateLimit();
  });

  test.afterAll(async ({ playwright }) => {
    if (original === undefined && roleId === undefined) return;
    const root = rootFixture();
    const ctx = await playwright.request.newContext({ ignoreHTTPSErrors: true });
    try {
      if (original !== undefined) {
        // オブジェクトストレージと分割アップロードは、元の値によらず必ず無効に戻す (admin_object_storage_save
        // と同じ理由。前の回の戻しが落ちた後の retry では、元の値として minio の値を読む)。
        // objectStorageSecretKey は admin/meta が伏せて返し、伏せた値は「変えない」として落とされるので、
        // 元に値があった場合は minio のものが残る。スタックの元の値は null で、無効にすれば使われない
        const restored = await callApi(ctx, 'admin/update-meta', {
          i: root.token, ...original, useObjectStorage: false, chunkedUploadEnabled: false,
        });
        // 落ちてもロールの削除まで送る (soft)
        expect.soft(restored.status(), `元の値に戻せなかった: ${await restored.text()}`).toBe(204);
      }
      if (roleId !== undefined) expect((await callApi(ctx, 'admin/roles/delete', { i: root.token, roleId })).status()).toBe(204);
    } finally {
      await ctx.dispose();
    }
  });

  test('start, append, finish; start, append, abort', async ({ request }) => {
    const root = rootFixture();
    // retry では、前の回の afterAll が戻した後の値を読む
    const beforeResp = await callApi(request, 'admin/meta', { i: root.token });
    // 読めなかったまま進むと、元の値が undefined になり、片付けが何も戻さない
    expect(beforeResp.ok(), 'admin/meta を読めなかった').toBe(true);
    const before = (await beforeResp.json()) as Record<string, unknown>;
    original = Object.fromEntries(Object.keys(STORAGE_META).map((k) => [k, before[k] ?? null]));
    const set = await callApi(request, 'admin/update-meta', { i: root.token, ...STORAGE_META });
    expect(set.status(), await set.text()).toBe(204);

    // 分割アップロードはロールのポリシー (canUseChunkedUpload) で許された人だけが使える
    const user = await signupUser(request, randomUsername('chunked'));
    const role = await callApi(request, 'admin/roles/create', {
      i: root.token, name: `pw-chunked-${Date.now()}`, description: '', color: null, iconUrl: null,
      target: 'manual', condFormula: {}, isPublic: false, isExplorable: false,
      isAdministrator: false, isModerator: false, asBadge: false,
      canEditMembersByModerator: false, displayOrder: 0,
      policies: { canUseChunkedUpload: { useDefault: false, priority: 0, value: true } },
    });
    expect(role.status(), await role.text()).toBe(200);
    roleId = ((await role.json()) as { id: string }).id;
    expect((await callApi(request, 'admin/roles/assign', { i: root.token, roleId, userId: user.id })).status()).toBe(204);

    // 2 つに分かれる大きさにして、途中の部分と最後の部分の両方を通す。中身は既定のポリシー
    // (uploadableFileTypes) が許すテキストにする。乱数のバイト列は application/octet-stream と
    // 判定され、UNALLOWED_FILE_TYPE で断られる
    const data = Buffer.from(randomBytes((CHUNK + 1024) / 2).toString('hex'));
    const session = await start(request, user, 'pw-chunked.txt', data.length);
    expect(session).toMatchObject({ chunkSize: CHUNK, totalChunks: 2 });

    const first = await append(request, user, session.uploadId, data, 0);
    expect(first).toEqual({ index: 0, next: 1, receivedBytes: CHUNK, totalSize: data.length, completed: false });
    const last = await append(request, user, session.uploadId, data, 1);
    expect(last).toEqual({ index: 1, next: 2, receivedBytes: data.length, totalSize: data.length, completed: true });

    const file = await callChecked<{ name: string; size: number; md5: string }>(
      request, 'drive/files/create-chunked/finish', { i: user.token, uploadId: session.uploadId },
    );
    // md5 は組み立てたオブジェクトを読み戻して計算する。部分の順や欠けがあれば変わる
    expect(file).toMatchObject({ name: 'pw-chunked.txt', size: data.length, md5: createHash('md5').update(data).digest('hex') });

    // 部分を 1 つ送ってから取り消す。S3 の側の未完了の multipart upload の破棄は、ここでは確かめない
    const aborted = await start(request, user, 'pw-aborted.txt', data.length);
    await append(request, user, aborted.uploadId, data, 0);
    await callChecked(request, 'drive/files/create-chunked/abort', { i: user.token, uploadId: aborted.uploadId });
    const gone = await callApi(request, 'drive/files/create-chunked/finish', { i: user.token, uploadId: aborted.uploadId });
    // 状態コードだけでは、別の理由の 404 (口の付け替えなど) でも通るので、エラーのコードまで見る
    expect(gone.status(), 'the aborted session is gone').toBe(404);
    expect(((await gone.json()) as { error: { code: string } }).error.code).toBe('NO_SUCH_UPLOAD_SESSION');
  });
});
