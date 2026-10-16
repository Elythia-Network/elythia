/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { collectStaticImports, staticImportClosure } from '../../../frontend-builder/locale-inliner/static-imports.js';

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

// boot.js は module ではないので、modulepreload を並べる部分を取り出して動かす。
function runPreloadRegion(globals: { CLIENT_ENTRY: string | null; CLIENT_PRELOADS?: unknown; lang: string }) {
	const source = readFileSync(join(__dirname, '../../public/loader/boot.js'), 'utf8');
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
