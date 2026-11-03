/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { loaderTarget, minifyLoaderAssets, minifyLoaderSource } from '../../../frontend-builder/minify-loader.js';

// 起動画面のスクリプトとCSSを、ビルドの出力でminifyする (#3968)。
const packagesDir = join(__dirname, '../../..');
const loaderPackages = ['frontend', 'frontend-embed'] as const;
const sourceOf = (pkg: string, name: string) => readFileSync(join(packagesDir, pkg, 'public/loader', name), 'utf8');

// espree は eslint の依存で、#3954 の検査 (shared/eslint.config.js) と同じものを使う
const espree = createRequire(createRequire(import.meta.url).resolve('eslint'))('espree');

// boot.js はサーバーが <script> で埋め込むので、閉じタグに化ける並びがあると
// internal/frontendutil の readInlinableAsset が埋め込みを諦める
const notInlinable = /<\/script|<\/style|<!--/i;

describe.each(loaderPackages)('%s loader', (pkg) => {
	it('minifies boot.js into a script the target browsers can parse', async () => {
		const source = sourceOf(pkg, 'boot.js');
		const minified = await minifyLoaderSource(source, 'js');
		expect(minified.length).toBeLessThan(source.length * 0.75);
		expect(() => espree.parse(minified, { ecmaVersion: 2024, sourceType: 'script' })).not.toThrow();
		expect(minified).not.toMatch(notInlinable);
		// サーバーが先に定義する定数は、名前を変えずに読む
		expect(minified).toContain('LANGS');
		expect(minified).toContain('CLIENT_ENTRY');
	});

	it('minifies style.css', async () => {
		const source = sourceOf(pkg, 'style.css');
		const minified = await minifyLoaderSource(source, 'css');
		expect(minified.length).toBeLessThan(source.length * 0.8);
		expect(minified).not.toMatch(/\/\*/);
		expect(minified).not.toMatch(notInlinable);
		expect(minified).toContain('#splash');
	});

	it('is wired into the build before the locale inliner', () => {
		const build = readFileSync(join(packagesDir, pkg, 'build.ts'), 'utf8');
		const body = build.slice(build.indexOf('async function build()'));
		expect(body).toMatch(/await viteBuild\(\);\s*await minifyLoaderAssets\(outputDir\);\s*await buildAllLocale\(\);/);
	});

	// target を vite の build.target と揃える。ずれると、vite が変換しない新しい構文を
	// minify が持ち込みうる
	it('uses the same browser target as vite', () => {
		const viteConfig = readFileSync(join(packagesDir, pkg, 'vite.config.ts'), 'utf8');
		const match = viteConfig.match(/build:\s*\{\s*target:\s*\[([^\]]*)\]/);
		expect(match).not.toBeNull();
		const viteTarget = [...match![1].matchAll(/'([^']+)'/g)].map(m => m[1]);
		expect(viteTarget).toEqual(loaderTarget);
	});
});

// 起動に失敗したときの画面 (renderError) が、minify の前後で同じに出ること。
// forceError を置くと、boot.js は最初に renderError を呼ぶ
async function renderForcedError(code: string): Promise<string> {
	const onerror = window.onerror;
	const onunhandledrejection = window.onunhandledrejection;
	window.document.body.innerHTML = '';
	window.document.head.querySelectorAll('style').forEach(el => el.remove());
	window.localStorage.setItem('forceError', '1');
	try {
		new Function('LANGS', 'CLIENT_ENTRY', 'CLIENT_PRELOADS', code)(['ja-JP', 'en-US'], null, []);
		for (let i = 0; i < 20 && window.document.getElementById('mkBootReload') == null; i++) {
			await new Promise(resolve => setTimeout(resolve, 10));
		}
		return window.document.body.innerHTML;
	} finally {
		window.localStorage.removeItem('forceError');
		window.onerror = onerror;
		window.onunhandledrejection = onunhandledrejection;
	}
}

describe.each(loaderPackages)('%s boot.js error screen', (pkg) => {
	afterEach(() => {
		window.document.body.innerHTML = '';
	});

	it('renders the same error screen after minification', async () => {
		const source = sourceOf(pkg, 'boot.js');
		const before = await renderForcedError(source);
		const after = await renderForcedError(await minifyLoaderSource(source, 'js'));
		expect(before).toContain('mkBootReload');
		expect(before).toContain('FORCED_ERROR');
		expect(after).toBe(before);
	});
});

describe('minifyLoaderAssets', () => {
	let dir: string | null = null;

	afterEach(() => {
		if (dir != null) rmSync(dir, { recursive: true, force: true });
		dir = null;
	});

	it('overwrites boot.js and style.css in the output directory', async () => {
		dir = mkdtempSync(join(tmpdir(), 'loader-minify-'));
		mkdirSync(join(dir, 'loader'));
		writeFileSync(join(dir, 'loader/boot.js'), '\'use strict\';\n// comment\n(() => {\n\tconst message = \'hello\';\n\tconsole.log(message);\n})();\n');
		writeFileSync(join(dir, 'loader/style.css'), '/* comment */\n#splash {\n\topacity: 1.0;\n}\n');
		await minifyLoaderAssets(dir);
		expect(readFileSync(join(dir, 'loader/boot.js'), 'utf8')).toBe('"use strict";console.log("hello");\n');
		expect(readFileSync(join(dir, 'loader/style.css'), 'utf8')).toBe('#splash{opacity:1}\n');
	});
});
