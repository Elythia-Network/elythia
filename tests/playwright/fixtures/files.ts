/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #829 PR-C: drive 系 spec で共有する fixture file。
//
// drive/files/create / find / find-by-hash / 等の spec で同じ test image を
// upload するため、binary を 1 か所に集約する。test 内で動的生成すると
// node の Sharp/Canvas 依存が増えるので、固定の minimal PNG を base64 で
// 持つ方が deterministic + 軽量。

import type { APIRequestContext } from '@playwright/test';

// 1x1 transparent PNG, 67 bytes。base64 decode 結果も deterministic で
// md5 (= file content hash) も常に同じになる = find-by-hash spec で
// 検証 anchor として使える。
export const tinyPNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
  'base64',
);

// drive/files/* response の最小 shape。upstream の packedDriveFileSchema
// で必須 field のうち identity + 基本 metadata のみ含む。md5 / blurhash /
// url / createdAt 等の詳細 field を assert する spec は本 interface を
// extends で拡張する (#855 PR-C で chat 添付 spec から共有開始)。
export interface DriveFile {
  id: string;
  name: string;
  type: string;
  size: number;
}

// uploadTinyPNG は test 用の 1x1 透過 PNG を /api/drive/files/create に
// multipart で upload する helper。drive 系 spec が複数で同 pattern を
// 抱えていたので集約 (#967 batch4 review)。
// 失敗時はキャッチ側で test を fail させたいので、status と body を
// そのまま返さず正常系で DriveFile を返し、異常系は throw する。
export async function uploadTinyPNG(
  request: APIRequestContext,
  baseURL: string,
  token: string,
  name: string,
): Promise<DriveFile> {
  const resp = await request.post(`${baseURL}/api/drive/files/create`, {
    ignoreHTTPSErrors: true,
    multipart: {
      i: token,
      file: {
        name,
        mimeType: 'image/png',
        buffer: tinyPNG,
      },
    },
  });
  if (resp.status() !== 200) {
    throw new Error(
      `drive/files/create failed: ${resp.status()} ${await resp.text()}`,
    );
  }
  const body = (await resp.json()) as DriveFile;
  if (!body.id) {
    throw new Error(`drive/files/create returned no id: ${JSON.stringify(body)}`);
  }
  return body;
}

// CRC32 lookup table for ZIP fixture builder (#882). standard polynomial
// 0xEDB88320 (= reflected 0x04C11DB7)。一度だけ計算して以降は table 参照。
const CRC32_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let i = 0; i < 256; i++) {
    let c = i;
    for (let j = 0; j < 8; j++) {
      c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    }
    table[i] = c >>> 0;
  }
  return table;
})();

function crc32(buf: Buffer): number {
  let crc = 0xffffffff;
  for (const b of buf) {
    crc = (crc >>> 8) ^ CRC32_TABLE[(crc ^ b) & 0xff];
  }
  return (crc ^ 0xffffffff) >>> 0;
}

// buildZip constructs a minimal valid ZIP archive (STORED, no compression)
// from the given entries. emoji import-zip 用 fixture (#882) の生成器。
// jszip 依存を増やさず、固定 entry 数 (meta.json + small PNG) なので
// CRC32 + LFH/CD/EOCD を手書きで組む。Go の archive/zip と stdlib unzip 両方で
// 読めることをローカルで確認している。
export function buildZip(entries: { name: string; content: Buffer }[]): Buffer {
  const localBlocks: Buffer[] = [];
  const centralBlocks: Buffer[] = [];
  let offset = 0;

  for (const e of entries) {
    const nameBuf = Buffer.from(e.name, 'utf-8');
    const crc = crc32(e.content);
    const size = e.content.length;

    // Local File Header (signature 0x04034b50, version 20 = ZIP 2.0)。
    const lfh = Buffer.alloc(30);
    lfh.writeUInt32LE(0x04034b50, 0);
    lfh.writeUInt16LE(20, 4); // version needed
    lfh.writeUInt16LE(0, 6); // general purpose bit flag
    lfh.writeUInt16LE(0, 8); // compression method (STORED)
    lfh.writeUInt16LE(0, 10); // mod time
    lfh.writeUInt16LE(0, 12); // mod date
    lfh.writeUInt32LE(crc, 14);
    lfh.writeUInt32LE(size, 18); // compressed size
    lfh.writeUInt32LE(size, 22); // uncompressed size
    lfh.writeUInt16LE(nameBuf.length, 26);
    lfh.writeUInt16LE(0, 28); // extra length
    localBlocks.push(lfh, nameBuf, e.content);

    // Central Directory Entry (signature 0x02014b50)。
    const cd = Buffer.alloc(46);
    cd.writeUInt32LE(0x02014b50, 0);
    cd.writeUInt16LE(20, 4); // version made by
    cd.writeUInt16LE(20, 6); // version needed
    cd.writeUInt16LE(0, 8); // flags
    cd.writeUInt16LE(0, 10); // compression
    cd.writeUInt16LE(0, 12); // mod time
    cd.writeUInt16LE(0, 14); // mod date
    cd.writeUInt32LE(crc, 16);
    cd.writeUInt32LE(size, 20);
    cd.writeUInt32LE(size, 24);
    cd.writeUInt16LE(nameBuf.length, 28);
    cd.writeUInt16LE(0, 30); // extra length
    cd.writeUInt16LE(0, 32); // comment length
    cd.writeUInt16LE(0, 34); // disk number start
    cd.writeUInt16LE(0, 36); // internal file attrs
    cd.writeUInt32LE(0, 38); // external file attrs
    cd.writeUInt32LE(offset, 42); // local header offset
    centralBlocks.push(cd, nameBuf);

    offset += 30 + nameBuf.length + size;
  }

  const cdBuf = Buffer.concat(centralBlocks);
  const cdSize = cdBuf.length;
  const cdOffset = offset;

  // End of Central Directory Record (signature 0x06054b50)。
  const eocd = Buffer.alloc(22);
  eocd.writeUInt32LE(0x06054b50, 0);
  eocd.writeUInt16LE(0, 4); // disk number
  eocd.writeUInt16LE(0, 6); // disk with central dir
  eocd.writeUInt16LE(entries.length, 8);
  eocd.writeUInt16LE(entries.length, 10);
  eocd.writeUInt32LE(cdSize, 12);
  eocd.writeUInt32LE(cdOffset, 16);
  eocd.writeUInt16LE(0, 20); // comment length

  return Buffer.concat([...localBlocks, cdBuf, eocd]);
}

// buildEmojiImportZip constructs a Misskey-compatible custom-emoji import ZIP
// (= meta.json + per-emoji PNG)。upstream の
// ExportCustomEmojisProcessorService が出力する metaDoc 構造に従う (#882)。
export function buildEmojiImportZip(
  emojis: { name: string; image?: Buffer }[],
): Buffer {
  const meta = {
    metaVersion: 2,
    host: null,
    exportedAt: new Date().toISOString(),
    emojis: emojis.map((e) => ({
      fileName: `${e.name}.png`,
      downloaded: true,
      emoji: {
        name: e.name,
        category: null,
        aliases: [],
        license: null,
        isSensitive: false,
        localOnly: false,
      },
    })),
  };
  const entries = [
    { name: 'meta.json', content: Buffer.from(JSON.stringify(meta), 'utf-8') },
    ...emojis.map((e) => ({
      name: `${e.name}.png`,
      content: e.image ?? tinyPNG,
    })),
  ];
  return buildZip(entries);
}
