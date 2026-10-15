/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Writes `<file>.br` (brotli, quality 10) and `<file>.gz` (gzip, level 9)
 * next to every compressible content-hashed file of the frontend build, for the server to
 * send as is (internal/server/frontend_static.go, #3593).
 *
 * Usage: node scripts/precompress.mjs <dir>...
 *
 * - The build itself does not run this. The server compresses on the fly when
 *   a variant is missing, so this can run after the server has been restarted
 *   on the new build (`make frontend-precompress`).
 * - Each variant is written to a temporary file and renamed into place, so the
 *   server never sees a half-written one.
 * - A variant newer than its source is kept, so a rerun only does the rest.
 * - A variant that would not be smaller than the source is not written.
 */

import * as fs from 'node:fs/promises';
import * as os from 'node:os';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import * as zlib from 'node:zlib';

const brotliCompress = promisify(zlib.brotliCompress);
const gzip = promisify(zlib.gzip);

/**
 * Names of content-hashed build files, the same rule as hashedAssetNameRe in
 * internal/server/frontend_static.go. Only these get variants: a file without
 * a hash changes in place, and the server does not use its variants (#3593).
 */
export const hashedNameRe = /^.+-[A-Za-z0-9_-]{8}\.[A-Za-z0-9]+$/;

/** Extensions of the build output worth compressing. */
export const compressibleExtensions = new Set(['.js', '.mjs', '.css', '.json', '.svg', '.html', '.txt', '.wasm']);

/**
 * Bodies smaller than this are sent uncompressed by the server anyway
 * (the gzip middleware's MinLength in internal/server).
 */
export const minSize = 1024;

const variants = [
	{
		suffix: '.br',
		// q11 は q10 より 1.6% 小さいだけで 2.3 倍遅い (ja-JP の 370 ファイルで実測、#3593)。
		compress: data => brotliCompress(data, {
			params: {
				[zlib.constants.BROTLI_PARAM_MODE]: zlib.constants.BROTLI_MODE_TEXT,
				[zlib.constants.BROTLI_PARAM_QUALITY]: 10,
				[zlib.constants.BROTLI_PARAM_SIZE_HINT]: data.length,
			},
		}),
	},
	{
		suffix: '.gz',
		compress: data => gzip(data, { level: zlib.constants.Z_BEST_COMPRESSION }),
	},
];

/**
 * @param {string} dir
 * @returns {Promise<string[]>}
 */
async function listFiles(dir) {
	const entries = await fs.readdir(dir, { recursive: true, withFileTypes: true });
	return entries
		.filter(entry => entry.isFile() && hashedNameRe.test(entry.name) && compressibleExtensions.has(path.extname(entry.name)))
		.map(entry => path.join(entry.parentPath, entry.name));
}

/**
 * @param {string} file
 * @returns {Promise<import('node:fs').Stats | null>}
 */
async function statOrNull(file) {
	try {
		return await fs.stat(file);
	} catch {
		return null;
	}
}

/**
 * Writes data to target through a temporary file in the same directory.
 *
 * @param {string} target
 * @param {Buffer} data
 */
async function writeAtomic(target, data) {
	const tmp = `${target}.tmp-${process.pid}-${Math.random().toString(36).slice(2)}`;
	try {
		await fs.writeFile(tmp, data);
		await fs.rename(tmp, target);
	} catch (err) {
		await fs.rm(tmp, { force: true });
		throw err;
	}
}

/**
 * @param {string} dir
 * @returns {Promise<{ files: number, written: number, kept: number }>}
 */
export async function precompressDir(dir) {
	const files = await listFiles(dir);
	let written = 0;
	let kept = 0;
	let next = 0;

	async function worker() {
		while (next < files.length) {
			const file = files[next++];
			const src = await fs.stat(file);
			if (src.size < minSize) continue;
			let data = null;
			for (const variant of variants) {
				const target = file + variant.suffix;
				const existing = await statOrNull(target);
				if (existing != null && existing.mtimeMs >= src.mtimeMs) {
					kept++;
					continue;
				}
				data ??= await fs.readFile(file);
				const out = await variant.compress(data);
				if (out.length >= data.length) continue;
				await writeAtomic(target, out);
				written++;
			}
		}
	}

	// zlib の非同期の処理は libuv の thread pool で走るので、並べる数は CPU の数に合わせる。
	await Promise.all(Array.from({ length: Math.max(1, os.availableParallelism()) }, () => worker()));
	return { files: files.length, written, kept };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
	const dirs = process.argv.slice(2);
	if (dirs.length === 0) {
		console.error('usage: node scripts/precompress.mjs <dir>...');
		process.exit(2);
	}
	for (const dir of dirs) {
		const started = Date.now();
		const { files, written, kept } = await precompressDir(dir);
		console.log(`precompress: ${dir}: ${files} files, ${written} variants written, ${kept} kept (${((Date.now() - started) / 1000).toFixed(1)}s)`);
	}
}
