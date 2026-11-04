/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { collectStaticImports, staticImportClosure } from '../../../frontend-builder/locale-inliner/static-imports.js';
import { LocaleInliner } from '../../../frontend-builder/locale-inliner.js';
import { blankLogger } from '../../../frontend-builder/logger.js';

// 起動の chunk を modulepreload で並べる (#3613)。
describe('collectStaticImports', () => {
	it('collects import / export-from statements of sibling chunks', () => {
		const code = 'import{a as e}from"./a.js";import"./b.js";export{c}from"./c.js";export*from"./d.js";import{x}from"vue";const f=()=>import("./lazy.js");';
		expect(collectStaticImports(code)).toEqual(['a.js', 'b.js', 'c.js', 'd.js']);
	});

	it('leaves out imports deleted by the locale inliner', () => {
		const imp = 'import{t as i}from"./i18n.js";';
		const code = `import{a}from"./a.js";${imp}console.log(a);`;
		const begin = code.indexOf(imp);
		expect(collectStaticImports(code, [{ begin, end: begin + imp.length }])).toEqual(['a.js']);
	});

	it('keeps imports outside the deleted ranges', () => {
		const code = 'import{a}from"./a.js";import{t}from"./i18n.js";';
		expect(collectStaticImports(code, [{ begin: 0, end: 5 }])).toEqual(['a.js', 'i18n.js']);
	});

	it('returns nothing for an empty chunk', () => {
		expect(collectStaticImports('')).toEqual([]);
	});
});

describe('staticImportClosure', () => {
	it('walks all depths breadth-first without the entry and without loops', () => {
		const graph: Record<string, string[]> = {
			'entry.js': ['a.js', 'b.js'],
			'a.js': ['c.js', 'entry.js'],
			'b.js': ['c.js'],
			'c.js': ['d.js'],
		};
		expect(staticImportClosure('entry.js', f => graph[f])).toEqual(['a.js', 'b.js', 'c.js', 'd.js']);
	});
});

// 埋め込みの entry が動き出してすぐ動的に読むもの (アイコンの CSS と ui.vue) も一覧に並べる (#3992)。
describe('LocaleInliner.collectPreloads with dynamic imports', () => {
	// embed の manifest の形 (vite 8)。`_i18n.js` は inline 化で消える import。
	const manifest = {
		'src/boot.ts': { file: 'scripts/entry-AAAAAAAA.js', src: 'src/boot.ts', isEntry: true, imports: ['_a.js', '_i18n.js'], dynamicImports: ['src/ui.vue'], css: ['assets/entry-AAAAAAAA.css'] },
		'src/i18n.ts': { file: 'scripts/i18n-IIIIIIII.js', src: 'src/i18n.ts', isEntry: true },
		'src/ui.vue': { file: 'scripts/ui-UUUUUUUU.js', src: 'src/ui.vue', isDynamicEntry: true, imports: ['_a.js', '_b.js', '_i18n.js'], css: ['assets/ui-UUUUUUUU.css'] },
		'_a.js': { file: 'scripts/a-BBBBBBBB.js', imports: ['_i18n.js'] },
		'_b.js': { file: 'scripts/b-CCCCCCCC.js', css: ['assets/b-CCCCCCCC.css'] },
		'_i18n.js': { file: 'scripts/i18n-IIIIIIII.js' },
		'../icons-subsetter/built/tabler-icons-frontendEmbed.css': { file: 'assets/icons-DDDDDDDD.css', src: '../icons-subsetter/built/tabler-icons-frontendEmbed.css' },
	};
	const sources: Record<string, string> = {
		'entry-AAAAAAAA.js': 'import{a}from"./a-BBBBBBBB.js";import{t}from"./i18n-IIIIIIII.js";const u=()=>import("./ui-UUUUUUUU.js");',
		'ui-UUUUUUUU.js': 'import{a}from"./a-BBBBBBBB.js";import{b}from"./b-CCCCCCCC.js";import{t}from"./i18n-IIIIIIII.js";',
		'a-BBBBBBBB.js': 'import{t}from"./i18n-IIIIIIII.js";export const a=1;',
		'b-CCCCCCCC.js': 'export const b=1;',
		'i18n-IIIIIIII.js': 'export const t=1;',
	};
	// locale inliner が i18n の import を消したのと同じ形にする
	const i18nImport = 'import{t}from"./i18n-IIIIIIII.js";';

	function inliner(): LocaleInliner {
		const li = new LocaleInliner({ outputDir: '/nonexistent', scriptsDir: 'scripts', i18nFile: 'src/i18n.ts', manifest: manifest as never, logger: blankLogger });
		for (const chunk of li.chunks) {
			const code = sources[chunk.fileName];
			chunk.sourceCode = code;
			const begin = code.indexOf(i18nImport);
			chunk.modifications = begin < 0 ? [] : [{ type: 'delete', begin, end: begin + i18nImport.length, localizedOnly: false }];
		}
		return li;
	}

	it('lists only the static closure without the option (the client build)', () => {
		expect(inliner().collectPreloads()['src/boot.ts']).toEqual(['scripts/a-BBBBBBBB.js']);
	});

	it('appends the dynamic chunk, its closure and the stylesheets', () => {
		const preloads = inliner().collectPreloads({
			dynamicImports: { 'src/boot.ts': ['icons-subsetter/built/tabler-icons-frontendEmbed.css', 'src/ui.vue'] },
		});
		expect(preloads['src/boot.ts']).toEqual([
			'scripts/a-BBBBBBBB.js',
			'assets/icons-DDDDDDDD.css',
			'scripts/ui-UUUUUUUU.js',
			'scripts/b-CCCCCCCC.js',
			'assets/ui-UUUUUUUU.css',
			'assets/b-CCCCCCCC.css',
		]);
		// i18n の chunk はブラウザが取りに行かないので並べない
		expect(preloads['src/boot.ts']).not.toContain('scripts/i18n-IIIIIIII.js');
	});

	// 依存を symlink で置くと、パッケージの外のファイルのキーは解決先からの相対パスになる
	it('finds a file outside the package by the end of its key', () => {
		const li = inliner();
		const key = '../../../../home/x/frontend/packages/icons-subsetter/built/tabler-icons-frontendEmbed.css';
		li.manifest = { ...manifest, [key]: manifest['../icons-subsetter/built/tabler-icons-frontendEmbed.css'] } as never;
		delete (li.manifest as Record<string, unknown>)['../icons-subsetter/built/tabler-icons-frontendEmbed.css'];
		expect(li.collectPreloads({ dynamicImports: { 'src/boot.ts': ['icons-subsetter/built/tabler-icons-frontendEmbed.css'] } })['src/boot.ts'])
			.toEqual(['scripts/a-BBBBBBBB.js', 'assets/icons-DDDDDDDD.css']);
	});

	// embed のビルドがこの一覧を書くこと (書かないと先読みが黙って消える)
	it('is written by the embed build for boot.ts', () => {
		const source = readFileSync(join(__dirname, '../../../frontend-embed/build.ts'), 'utf8');
		const call = source.match(/inliner\.savePreloads\(([\s\S]*?)\);/);
		expect(call?.[1]).toContain("'src/boot.ts': ['icons-subsetter/built/tabler-icons-frontendEmbed.css', 'src/ui.vue']");
	});

	it('fails the build when a named import is missing or ambiguous', () => {
		const li = inliner();
		li.manifest = { ...manifest, 'other/icons-subsetter/built/tabler-icons-frontendEmbed.css': { file: 'assets/x-XXXXXXXX.css' } } as never;
		expect(() => li.collectPreloads({ dynamicImports: { 'src/boot.ts': ['icons-subsetter/built/tabler-icons-frontendEmbed.css'] } })).toThrow(/more than one/);
		expect(() => inliner().collectPreloads({ dynamicImports: { 'src/boot.ts': ['src/missing.vue'] } })).toThrow(/missing\.vue/);
		expect(() => inliner().collectPreloads({ dynamicImports: { 'src/other.ts': ['src/ui.vue'] } })).toThrow(/other\.ts/);
	});
});

// boot.js は module ではないので、modulepreload を並べる部分を取り出して動かす。
function runPreloadRegion(globals: { CLIENT_ENTRY: string | null; CLIENT_PRELOADS?: unknown; lang: string }, pkg: 'frontend' | 'frontend-embed' = 'frontend') {
	const source = readFileSync(join(__dirname, `../../../${pkg}/public/loader/boot.js`), 'utf8');
	const match = source.match(/\/\/#region Preload\n([\s\S]*?)\/\/#endregion/);
	if (match == null) throw new Error('Preload region not found in boot.js');
	const head = window.document.createElement('head');
	const fakeDocument = { head, createElement: (tag: string) => window.document.createElement(tag) };
	new Function('CLIENT_ENTRY', 'CLIENT_PRELOADS', 'lang', 'document', match[1])(
		globals.CLIENT_ENTRY, globals.CLIENT_PRELOADS, globals.lang, fakeDocument,
	);
	return Array.from(head.querySelectorAll('link')).map(l => [l.getAttribute('rel'), l.getAttribute('href')]);
}

describe('boot.js preload', () => {
	it('preloads the entry and the listed chunks under the locale directory', () => {
		expect(runPreloadRegion({
			CLIENT_ENTRY: 'scripts/entry-AAAAAAAA.js',
			CLIENT_PRELOADS: ['scripts/a-BBBBBBBB.js', 'scripts/b-CCCCCCCC.js'],
			lang: 'ja-JP',
		})).toEqual([
			['modulepreload', '/vite/ja-JP/entry-AAAAAAAA.js'],
			['modulepreload', '/vite/ja-JP/a-BBBBBBBB.js'],
			['modulepreload', '/vite/ja-JP/b-CCCCCCCC.js'],
		]);
	});

	it('does nothing in dev mode or without the list', () => {
		expect(runPreloadRegion({ CLIENT_ENTRY: null, CLIENT_PRELOADS: ['scripts/a-BBBBBBBB.js'], lang: 'ja-JP' })).toEqual([]);
		expect(runPreloadRegion({ CLIENT_ENTRY: 'scripts/entry-AAAAAAAA.js', CLIENT_PRELOADS: undefined, lang: 'ja-JP' })).toEqual([]);
	});
});

// 埋め込みの boot.js も同じく並べる (#3992)。chunk はロケールごとのディレクトリにあるので、
// サーバーが渡す `scripts/...` に boot.js が決めた lang を当てはめる。
describe('embed boot.js preload', () => {
	it('preloads the entry and the listed chunks under the locale directory', () => {
		expect(runPreloadRegion({
			CLIENT_ENTRY: 'scripts/entry-AAAAAAAA.js',
			CLIENT_PRELOADS: ['scripts/a-BBBBBBBB.js', 'scripts/ui-CCCCCCCC.js'],
			lang: 'en-US',
		}, 'frontend-embed')).toEqual([
			['modulepreload', '/embed_vite/en-US/entry-AAAAAAAA.js'],
			['modulepreload', '/embed_vite/en-US/a-BBBBBBBB.js'],
			['modulepreload', '/embed_vite/en-US/ui-CCCCCCCC.js'],
		]);
	});

	it('does nothing in dev mode or without the list', () => {
		expect(runPreloadRegion({ CLIENT_ENTRY: null, CLIENT_PRELOADS: ['scripts/a-BBBBBBBB.js'], lang: 'ja-JP' }, 'frontend-embed')).toEqual([]);
		expect(runPreloadRegion({ CLIENT_ENTRY: 'scripts/entry-AAAAAAAA.js', CLIENT_PRELOADS: undefined, lang: 'ja-JP' }, 'frontend-embed')).toEqual([]);
	});

	// 先読みは entry の import より前に並べる (後ろだと import の取得と重ならない)
	it('runs before the entry is imported', () => {
		const source = readFileSync(join(__dirname, '../../../frontend-embed/public/loader/boot.js'), 'utf8');
		expect(source.indexOf('//#region Preload')).toBeGreaterThan(source.indexOf('//#region Detect language'));
		expect(source.indexOf('//#region Preload')).toBeLessThan(source.indexOf('//#region Script'));
	});
});
