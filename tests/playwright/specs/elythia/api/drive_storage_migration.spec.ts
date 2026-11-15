/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ドライブのファイルのオブジェクトストレージへの移行 (#4179) の 6 つの口を 2xx まで通し、
// 応答を /api.json の宣言で照合する (#4184)。ローカルに置いたファイルを、compose.yml の minio へ
// 移す → 確かめる → ローカルを消す → ローカルへ戻す → バケットの写しを消す (#4197) の順に流し、
// そのたびに同じ URL で同じ中身を配ることを見る。
//
// 移す処理はインスタンスの全てのファイルが対象になる。ローカルを消したまま保存先を戻すと、この
// shard の後の spec のファイルが配れなくなるので、最後に必ずローカルへ戻してから、オブジェクト
// ストレージを無効に戻す (片付けは afterAll で、新しい context から送る)。
//
// ローカルを消す口は DB のバックアップと同じ再認証を求める。root に 2FA を付けると他の spec の
// 入り方が変わるので、2FA と管理者のロールは、このテストで作った利用者に付ける。

import { randomBytes } from 'node:crypto';
import { expect, test, type APIRequestContext } from '@playwright/test';
import { authenticator } from 'otplib';
import { baseURL, callApi } from '../../../fixtures/api';
import { callChecked } from '../../../fixtures/api_schema';
import { assignRole, expectApiOk, rootFixture } from '../../../fixtures/admin_table';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';

// compose.yml の minio / minio-init と揃える (drive_chunked と同じ。prefix だけ分ける)
const STORAGE_META = {
  useObjectStorage: true,
  objectStorageBucket: 'drive',
  objectStoragePrefix: 'pw-migration',
  objectStorageEndpoint: 'minio.local',
  objectStoragePort: 9000,
  objectStorageRegion: 'us-east-1',
  objectStorageAccessKey: 'minioadmin',
  objectStorageSecretKey: 'minioadmin-secret',
  objectStorageUseSSL: false,
  objectStorageS3ForcePathStyle: true,
  objectStorageSetPublicRead: false,
};

interface Status {
  objectStorageEnabled: boolean;
  running: string | null;
  localFiles: number;
  migratedFiles: number;
  verifiedFiles: number;
  purgeableFiles: number;
  localDeletedFiles: number;
  restoredCopies: number;
}

const STATUS = 'admin/drive/object-storage-migration/status';

// 移す処理はこの shard でそれまでに上がった全てのファイルが対象になるので、待つ時間を件数で
// 見積もる。件数の少ない shard では、これまでと同じ 60 秒になる
const IDLE_BASE_MS = 60_000;
const IDLE_PER_FILE_MS = 100;
const IDLE_MAX_MS = 300_000;

/** The time to wait for one operation over `files` files. */
function idleTimeout(files: number): number {
  return Math.min(IDLE_BASE_MS + files * IDLE_PER_FILE_MS, IDLE_MAX_MS);
}

/** Reads the status until no operation runs, and returns it. */
async function waitIdle(request: APIRequestContext, token: string, files = 0): Promise<Status> {
  let st: Status | undefined;
  await expect.poll(async () => {
    st = await callChecked<Status>(request, STATUS, { i: token });
    return st.running;
  }, { timeout: idleTimeout(files), intervals: [500, 1000, 2000] }).toBeNull();
  return st!;
}

/** Fetches the drive file at url and returns its bytes. */
async function fetchFile(request: APIRequestContext, url: string): Promise<Buffer> {
  // 移した後も url の列は /files/<accessKey> のまま。ホストは spec のつなぎ先へ読み替える
  const resp = await request.get(`${baseURL()}${new URL(url).pathname}`, { failOnStatusCode: false });
  expect(resp.status(), `${url} を配らない`).toBe(200);
  return resp.body();
}

test.describe('drive: moving local files to object storage and back', () => {
  test.skip(isTsBackend, 'オブジェクトストレージへの移行は Elythia 独自');
  // test の中で waitIdle を 5 回呼ぶ
  test.setTimeout(5 * IDLE_MAX_MS);

  let original: Record<string, unknown> | undefined;
  let roleId: string | undefined;
  // テストの中で消すファイル。途中で落ちたら afterAll で消す (初めからオブジェクトストレージへ
  // 上げたファイルを残したまま無効に戻すと、その行が配れなくなる)
  const leftovers = new Set<string>();

  test.beforeAll(() => {
    resetRateLimit();
  });

  test.afterAll(async ({ playwright }, testInfo) => {
    if (original === undefined && roleId === undefined) return;
    // フックの時間切れは describe の setTimeout を受け継がないことがあるので、ここで決める
    testInfo.setTimeout(3 * IDLE_MAX_MS);
    const root = rootFixture();
    const ctx = await playwright.request.newContext({ ignoreHTTPSErrors: true });
    try {
      // ロールは保存先と関係なく消す。下の検査で止まっても残さない
      if (roleId !== undefined) expect.soft((await callApi(ctx, 'admin/roles/delete', { i: root.token, roleId })).status()).toBe(204);
      for (const fileId of leftovers) {
        await callApi(ctx, 'drive/files/delete', { i: root.token, fileId });
      }
      if (original !== undefined) {
        // 途中で落ちて移したファイルが残っていたら、保存先を戻す前にローカルへ戻す。戻さずに無効に
        // すると、ローカルを消したファイルが配れなくなる。status が読めないときも、無効にはしない
        // (waitIdle が投げて、無効にする前に止まる)
        let st = await waitIdle(ctx, root.token, IDLE_MAX_MS / IDLE_PER_FILE_MS);
        if (st.migratedFiles > 0) {
          await callChecked(ctx, 'admin/drive/object-storage-migration/restore', { i: root.token });
          st = await waitIdle(ctx, root.token, st.migratedFiles);
        }
        // 戻せなかったファイルが残ったまま無効にすると、それが配れなくなり、後の spec に響く
        expect(st.migratedFiles, `ローカルへ戻せなかったファイル: ${JSON.stringify(st)}`).toBe(0);
        // 戻した写しの記録が残っていたら、オブジェクトストレージが有効なうちに消す (#4197)
        if (st.restoredCopies > 0) {
          await callChecked(ctx, 'admin/drive/object-storage-migration/cleanup-copies', { i: root.token });
          st = await waitIdle(ctx, root.token, st.restoredCopies);
        }
        const restored = await callApi(ctx, 'admin/update-meta', { i: root.token, ...original, useObjectStorage: false });
        expect.soft(restored.status(), `元の値に戻せなかった: ${await restored.text()}`).toBe(204);
      }
    } finally {
      await ctx.dispose();
    }
  });

  test('move, verify, delete the local copies, move back, delete the copies left', async ({ request }) => {
    const root = rootFixture();
    const upload = async (name: string, content: Buffer) => {
      const up = await request.post(`${baseURL()}/api/drive/files/create`, {
        multipart: { i: root.token, file: { name, mimeType: 'text/plain', buffer: content } },
        failOnStatusCode: false,
      });
      expect(up.status(), await up.text()).toBe(200);
      return (await up.json()) as { id: string; url: string };
    };
    // オブジェクトストレージを無効のまま上げ、ローカルに置く。gone は戻した後に消す
    const body = Buffer.from(randomBytes(512).toString('hex'));
    const file = await upload('pw-migration.txt', body);
    const gone = await upload('pw-migration-gone.txt', Buffer.from(randomBytes(64).toString('hex')));
    leftovers.add(gone.id);

    const beforeResp = await callApi(request, 'admin/meta', { i: root.token });
    expect(beforeResp.ok(), 'admin/meta を読めなかった').toBe(true);
    const before = (await beforeResp.json()) as Record<string, unknown>;
    original = Object.fromEntries(Object.keys(STORAGE_META).map((k) => [k, before[k] ?? null]));
    const set = await callApi(request, 'admin/update-meta', { i: root.token, ...STORAGE_META });
    expect(set.status(), await set.text()).toBe(204);

    let st = await waitIdle(request, root.token);
    expect(st.objectStorageEnabled).toBe(true);
    expect(st.localFiles).toBeGreaterThan(0);
    const files = st.localFiles;
    // 初めからオブジェクトストレージへ上げたファイル。移す・戻す・写しを消すのどれにも入らない
    const directBody = Buffer.from(randomBytes(64).toString('hex'));
    const direct = await upload('pw-migration-direct.txt', directBody);
    leftovers.add(direct.id);

    // 移す。url は変わらず、同じ中身を配る
    await callChecked(request, 'admin/drive/object-storage-migration/start', { i: root.token });
    st = await waitIdle(request, root.token, files);
    expect(st.migratedFiles).toBeGreaterThan(0);
    expect((await fetchFile(request, file.url)).equals(body)).toBe(true);

    // 確かめる
    await callChecked(request, 'admin/drive/object-storage-migration/verify', { i: root.token });
    st = await waitIdle(request, root.token, files);
    expect(st.verifiedFiles).toBeGreaterThan(0);
    expect(st.purgeableFiles).toBeGreaterThan(0);

    // ローカルを消す。2FA を登録した管理者で、パスワードとワンタイムのコードで再認証する
    const admin = await signupUser(request, randomUsername('pwmig'), DEFAULT_TEST_PASSWORD);
    roleId = await assignRole(request, root, admin.id, { isAdministrator: true });
    const { secret } = await expectApiOk(request, 'i/2fa/register', { i: admin.token, password: DEFAULT_TEST_PASSWORD }) as { secret: string };
    await expectApiOk(request, 'i/2fa/done', { i: admin.token, token: authenticator.generate(secret) });
    // 再認証が無ければ断る
    const refused = await callApi(request, 'admin/drive/object-storage-migration/purge-local', { i: admin.token, password: 'wrong' });
    expect(refused.status(), await refused.text()).toBe(403);
    // 再認証で断ったこと (ロールや token の 403 でないこと) まで見る
    expect(((await refused.json()) as { error: { code: string } }).error.code).toMatch(/^REAUTHENTICATION_(REQUIRED|FAILED)$/);
    // 登録で使ったコードは使用済みになる。サーバーは前後 1 ステップを許すので、次のステップのコードを作る
    const next = authenticator.clone({ epoch: Date.now() + 30_000 }).generate(secret);
    await callChecked(request, 'admin/drive/object-storage-migration/purge-local', { i: admin.token, password: DEFAULT_TEST_PASSWORD, token: next });
    st = await waitIdle(request, root.token, files);
    expect(st.localDeletedFiles).toBeGreaterThan(0);
    // ローカルを消しても、オブジェクトストレージから同じ URL で配る
    expect((await fetchFile(request, file.url)).equals(body)).toBe(true);

    // ローカルへ戻す
    await callChecked(request, 'admin/drive/object-storage-migration/restore', { i: root.token });
    st = await waitIdle(request, root.token, files);
    expect(st.migratedFiles, '移したファイルが残った').toBe(0);
    expect((await fetchFile(request, file.url)).equals(body)).toBe(true);

    // 戻したファイルの写しはバケットに残り、記録される (#4197)。戻した後にファイルを消すと、
    // その写しも消して記録を外す (テキストのファイルはサムネイルが無いので 1 件)
    expect(st.restoredCopies).toBeGreaterThan(0);
    const copiesBefore = st.restoredCopies;
    await expectApiOk(request, 'drive/files/delete', { i: root.token, fileId: gone.id });
    leftovers.delete(gone.id);
    await expect.poll(async () => (await callChecked<Status>(request, STATUS, { i: root.token })).restoredCopies,
      { message: '消したファイルの写しの記録が残った', timeout: 30_000 }).toBe(copiesBefore - 1);

    // 初めから上げたファイルの url はバケットの URL なので、アクセスキー (url の最後) で /files/ から読む
    const directKey = new URL(direct.url).pathname.split('/').pop()!;
    const directURL = `${baseURL()}/files/${directKey}`;
    expect((await fetchFile(request, directURL)).equals(directBody)).toBe(true);

    // 写しを消す。戻したファイルはローカルから、初めから上げたファイルはオブジェクトストレージから
    // 同じ URL で配り続ける
    await callChecked(request, 'admin/drive/object-storage-migration/cleanup-copies', { i: root.token });
    st = await waitIdle(request, root.token, copiesBefore);
    expect(st.restoredCopies, '写しの記録が残った').toBe(0);
    expect((await fetchFile(request, file.url)).equals(body)).toBe(true);
    expect((await fetchFile(request, directURL)).equals(directBody), '初めから上げたファイルが消えた').toBe(true);
    await expectApiOk(request, 'drive/files/delete', { i: root.token, fileId: direct.id });
    leftovers.delete(direct.id);
  });
});
