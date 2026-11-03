/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import * as fs from 'node:fs/promises';
import * as path from 'node:path';
import { transform } from 'esbuild';

/**
 * Browsers the loader must run on. Same as `build.target` in the vite configs
 * of frontend and frontend-embed.
 */
export const loaderTarget = ['chrome130', 'firefox132', 'safari18.2'];

/**
 * Minifies the source of a loader asset.
 *
 * @param code Source of `loader/boot.js` or `loader/style.css`.
 * @param loader `js` for the script, `css` for the stylesheet.
 */
export async function minifyLoaderSource(code: string, loader: 'js' | 'css'): Promise<string> {
	const result = await transform(code, {
		loader,
		minify: true,
		target: loaderTarget,
		// 日本語などを \u のエスケープにすると大きくなるだけなので、UTF-8 のまま出す
		charset: 'utf8',
		legalComments: 'none',
	});
	return result.code;
}

/**
 * Minifies `loader/boot.js` and `loader/style.css` in the build output in place.
 *
 * @param outputDir The vite output directory (e.g. `built/_frontend_vite_`).
 */
export async function minifyLoaderAssets(outputDir: string): Promise<void> {
	// loader は public/ からそのまま写されるので、vite の minify を通らない。サーバーが
	// 全てのページの HTML に埋め込むので、コメントと字下げの分だけ毎回の転送が増えていた (#3968)。
	// ソース (public/loader/) は読める形のまま残し、ESLint もソースを見る (#3954)
	const files = [['boot.js', 'js'], ['style.css', 'css']] as const;
	for (const [name, loader] of files) {
		const file = path.join(outputDir, 'loader', name);
		const source = await fs.readFile(file, 'utf8');
		await fs.writeFile(file, await minifyLoaderSource(source, loader));
	}
}
