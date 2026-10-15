/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, utimesSync, writeFileSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { brotliDecompressSync, gunzipSync } from 'node:zlib';
import { precompressDir } from './precompress.mjs';

function makeBuild() {
	const dir = mkdtempSync(join(tmpdir(), 'precompress-'));
	mkdirSync(join(dir, 'ja-JP'));
	writeFileSync(join(dir, 'ja-JP', 'a-12345678.js'), 'console.log("x");\n'.repeat(200));
	// 圧縮は効くが、サーバーがどうせ圧縮しない大きさ。
	writeFileSync(join(dir, 'small-12345678.js'), 'x();'.repeat(250));
	// 大きいが縮まない (乱数)。
	writeFileSync(join(dir, 'random-12345678.js'), randomBytes(4096));
	writeFileSync(join(dir, 'image.png'), Buffer.alloc(4096));
	// hash の無いファイルには作らない (サーバーも使わない)。
	writeFileSync(join(dir, 'manifest.json'), JSON.stringify({ a: 'b'.repeat(4096) }));
	return dir;
}

test('writes brotli and gzip variants that decode to the source', async (t) => {
	const dir = makeBuild();
	t.after(() => rmSync(dir, { recursive: true, force: true }));

	const res = await precompressDir(dir);
	const src = readFileSync(join(dir, 'ja-JP', 'a-12345678.js'));
	assert.deepEqual(brotliDecompressSync(readFileSync(join(dir, 'ja-JP', 'a-12345678.js.br'))), src);
	assert.deepEqual(gunzipSync(readFileSync(join(dir, 'ja-JP', 'a-12345678.js.gz'))), src);
	assert.equal(res.written, 2);
	// 小さいファイル・縮まないファイル・hash の無いファイル・圧縮の対象でない拡張子には作らない。一時ファイルも残さない。
	assert.deepEqual(readdirSync(dir).sort(), ['image.png', 'ja-JP', 'manifest.json', 'random-12345678.js', 'small-12345678.js']);
	assert.deepEqual(readdirSync(join(dir, 'ja-JP')).sort(), ['a-12345678.js', 'a-12345678.js.br', 'a-12345678.js.gz']);
});

test('a rerun keeps variants newer than the source and redoes stale ones', async (t) => {
	const dir = makeBuild();
	t.after(() => rmSync(dir, { recursive: true, force: true }));
	const file = join(dir, 'ja-JP', 'a-12345678.js');

	await precompressDir(dir);
	const again = await precompressDir(dir);
	assert.equal(again.written, 0);
	assert.equal(again.kept, 2);

	// 元が新しくなったら作り直す。
	writeFileSync(file, 'console.log("y");\n'.repeat(300));
	const future = new Date(statSync(file + '.br').mtimeMs + 10_000);
	utimesSync(file, future, future);
	const redo = await precompressDir(dir);
	assert.equal(redo.written, 2);
	assert.deepEqual(brotliDecompressSync(readFileSync(file + '.br')), readFileSync(file));
});
