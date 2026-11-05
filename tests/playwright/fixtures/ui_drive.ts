/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ドライブ (MkDrive) を UI で扱う spec で共有するヘルパ (#4049)。
//
// ドライブは同じ中身のファイルを上げ直すと、前に上げたもの (前の名前) を返す。1 つの
// spec で複数のファイルを上げるときは、中身を変える (`distinctPNG`)。PNG の先頭
// の IHDR の後ろに、中身の違う tEXt のチャンクを足す (画像としては同じ 1x1 のまま)。
// IEND の後ろにバイトを足す形は、ブラウザの上げる前の処理 (プレビューの生成) が
// 終わらず、「Upload」が押せないままになった。
//
// ファイルとフォルダは data-testid (`drive-file` / `drive-folder`) と名前で、ちょうど
// 1 つのときだけ取る。

import { randomBytes } from 'node:crypto';
import type { APIRequestContext, Page } from '@playwright/test';
import { type DriveFile, tinyPNG } from './files';
import { clickWhenReady, type ClickOptions } from './ui_click';

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let i = 0; i < 256; i++) {
    let c = i;
    for (let j = 0; j < 8; j++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[i] = c >>> 0;
  }
  return t;
})();

function crc32(buf: Buffer): number {
  let c = 0xffffffff;
  for (const b of buf) c = (c >>> 8) ^ CRC_TABLE[(c ^ b) & 0xff];
  return (c ^ 0xffffffff) >>> 0;
}

// PNG の署名 (8) と IHDR のチャンク (長さ 4 + 型 4 + 中身 13 + CRC 4) の後ろ
const AFTER_IHDR = 8 + 4 + 4 + 13 + 4;

/** A valid 1x1 PNG whose bytes (and so md5) differ on every call. */
export function distinctPNG(): Buffer {
  const typeAndData = Buffer.concat([Buffer.from('tEXt'), Buffer.from(`Comment\0${randomBytes(8).toString('hex')}`)]);
  const len = Buffer.alloc(4);
  len.writeUInt32BE(typeAndData.length - 4);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(typeAndData));
  return Buffer.concat([tinyPNG.subarray(0, AFTER_IHDR), len, typeAndData, crc, tinyPNG.subarray(AFTER_IHDR)]);
}

/** Uploads `buffer` as a PNG named `name` through the API. */
export async function uploadPNG(request: APIRequestContext, baseURL: string, token: string, name: string, buffer: Buffer): Promise<DriveFile> {
  const resp = await request.post(`${baseURL}/api/drive/files/create`, {
    ignoreHTTPSErrors: true,
    multipart: { i: token, file: { name, mimeType: 'image/png', buffer } },
  });
  if (resp.status() !== 200) throw new Error(`drive/files/create failed: ${resp.status()} ${await resp.text()}`);
  return (await resp.json()) as DriveFile;
}

/** Clicks the only drive item (`drive-file` or `drive-folder`) whose name is `name`. */
export function clickDriveItem(page: Page, kind: 'drive-file' | 'drive-folder', name: string, options?: ClickOptions): Promise<void> {
  return clickWhenReady(
    page,
    `${kind}「${name}」(ちょうど 1 つ)`,
    ({ k, n }: { k: string; n: string }) => {
      // ファイルの名前は拡張子を別の span に分けて出す (MkDrive.file.vue) ので、文字全体で比べる
      const hits = Array.from(document.querySelectorAll(`[data-testid="${k}"]`))
        .filter((el) => (el.textContent ?? '').replace(/\s+/g, '') === n.replace(/\s+/g, ''));
      return hits.length === 1 ? hits[0] : null;
    },
    { k: kind, n: name },
    options,
  );
}

/** Counts the drive items (`drive-file` or `drive-folder`) whose name is `name`. */
export function countDriveItems(page: Page, kind: 'drive-file' | 'drive-folder', name: string): Promise<number> {
  return page.evaluate(({ k, n }) => Array.from(document.querySelectorAll(`[data-testid="${k}"]`))
    .filter((el) => (el.textContent ?? '').replace(/\s+/g, '') === n.replace(/\s+/g, '')).length, { k: kind, n: name });
}
