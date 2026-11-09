/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// バックアップの世代のダウンロードと削除 (#3462) を叩き、応答を /api.json の宣言で照合する
// (#4093)。このスタックには世代を作るデーモンが無いので、世代のファイル (meta.json と
// ダンプ) を runner から保存先に置く。保存先は compose.yml で mkgo の /backup と共有して
// いる。置いた世代は削除の API で消え、落ちたときも finally で消す。残すと、世代が無い
// ことを確かめる admin_backups が落ちる。

import { createHash, randomBytes } from 'node:crypto';
import { existsSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { callChecked } from '../../../fixtures/api_schema';
import { withDb } from '../../../fixtures/db';
import { rootFixture } from '../../../fixtures/admin_table';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { totpAdmin } from '../../../fixtures/reauth';

// runner から見た保存先 (compose.yml の ./backup-fixture)
const STORAGE = 'backup-fixture';
const DUMP_FILE = 'dump.pgdump';

/** A generation id (internal/backup/layout.go: 20060102T150405Z) of `at`. */
function generationId(at: Date): string {
  return at.toISOString().replace(/[-:]/g, '').replace(/\.\d{3}Z$/, 'Z');
}

/** Places a generation with a small dump and returns its id, its files and the bytes it takes. */
function placeGeneration(): { id: string; dir: string; dump: Buffer; bytes: number } {
  const id = generationId(new Date());
  const dir = join(STORAGE, 'generations', id);
  const dump = randomBytes(64);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, DUMP_FILE), dump);
  // meta.json の形は internal/backup/meta.go。読むときに必須なのは formatVersion と id の一致で、
  // ダウンロードは dumpFile と encrypted を、一覧は createdAt や dumpSize も使う
  const meta = JSON.stringify({
    formatVersion: 1,
    id,
    createdAt: new Date().toISOString(),
    dumpFile: DUMP_FILE,
    dumpSize: dump.length,
    dumpSha256: createHash('sha256').update(dump).digest('hex'),
    plainSha256: createHash('sha256').update(dump).digest('hex'),
    encrypted: false,
  });
  writeFileSync(join(dir, 'meta.json'), meta);
  return { id, dir, dump, bytes: dump.length + Buffer.byteLength(meta) };
}

test.describe('admin: the backup generations, downloaded and deleted', () => {
  test.skip(isTsBackend, 'バックアップは Elythia 独自');
  test.setTimeout(90_000);

  test.beforeAll(() => {
    resetRateLimit();
    // 前の実行が timeout などで finally まで届かずに残した世代を消す。残すと、世代が無いことを
    // 確かめる admin_backups が落ち続ける (中のファイルは root の持ち物で、手元では消しにくい)
    rmSync(join(STORAGE, 'generations'), { recursive: true, force: true });
  });

  test('download a generation, then delete it', async ({ request }) => {
    const root = rootFixture();
    let generation: ReturnType<typeof placeGeneration> | undefined;
    let roleId: string | undefined;
    try {
      generation = placeGeneration();
      const reauth = await totpAdmin(request, root, 'admbkd');
      roleId = reauth.roleId;
      const auth = { i: reauth.admin.token, password: reauth.password };

      // 前の段のコードはサーバーの段が進むと通らなくなるので、先に使う。日時の書式は #4091 で直す
      const download = await callChecked<{ url: string; size: number; fileName: string; via: string; encrypted: boolean }>(
        request, 'admin/backup/download', { ...auth, token: reauth.code(-1), id: generation.id }, { knownBug: '#4091' },
      );
      expect(download).toMatchObject({ size: generation.dump.length, fileName: `${generation.id}-${DUMP_FILE}`, via: 'server', encrypted: false });
      const fetched = await request.get(download.url, { failOnStatusCode: false });
      expect(fetched.status(), 'the download URL serves the dump').toBe(200);
      expect(Buffer.compare(await fetched.body(), generation.dump)).toBe(0);

      const deleted = await callChecked<{ freedBytes: number }>(request, 'admin/backup/delete', { ...auth, token: reauth.code(1), id: generation.id });
      // meta.json だけ消して止まる削除も見逃さないよう、世代のファイル全部の大きさと、ディレクトリごと消えたことを見る
      expect(deleted.freedBytes).toBe(generation.bytes);
      expect(existsSync(generation.dir), 'the generation is gone from the storage').toBe(false);
    } finally {
      if (generation !== undefined) rmSync(generation.dir, { recursive: true, force: true });
      if (roleId !== undefined) expect.soft((await callApi(request, 'admin/roles/delete', { i: root.token, roleId })).status()).toBe(204);
    }
  });

  // パスキーでの再認証のチャレンジ。登録はブラウザの認証器が要るので、鍵の行を DB に直に
  // 入れる。チャレンジを作るときに読むのは鍵の id と、デコードできる公開鍵
  // (internal/core/twofactor/webauthn.go の userAdapter。どちらも base64url で、読めない行は
  // 落とされる)。公開鍵の値は検証のときにしか使わないので、base64url の乱数でよい
  test('the challenge to re-authenticate with a passkey', async ({ request }) => {
    const root = rootFixture();
    const reauth = await totpAdmin(request, root, 'admbkc');
    const keyId = randomBytes(16).toString('base64url');
    try {
      await withDb((db) => db.query(
        'INSERT INTO user_security_key (id, "userId", name, "publicKey") VALUES ($1, $2, $3, $4)',
        [keyId, reauth.admin.id, 'pw key', randomBytes(32).toString('base64url')],
      ));
      const challenge = await callChecked<{ challenge: string; allowCredentials?: { type: string; id: string }[] }>(
        request, 'admin/backup/reauth-challenge', { i: reauth.admin.token },
      );
      expect(challenge.challenge.length).toBeGreaterThan(0);
      expect(challenge.allowCredentials).toEqual([expect.objectContaining({ type: 'public-key', id: keyId })]);
    } finally {
      await withDb((db) => db.query('DELETE FROM user_security_key WHERE id = $1', [keyId]));
      expect.soft((await callApi(request, 'admin/roles/delete', { i: root.token, roleId: reauth.roleId })).status()).toBe(204);
    }
  });
});
