/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, test } from 'vitest';
import * as mfm from '@/utility/mfm.js';

describe('parse', () => {
	test('parses ordinary input as mfm-js does', () => {
		expect(mfm.parse('**a** :b: @c')).toEqual([
			{ type: 'bold', children: [{ type: 'text', props: { text: 'a' } }] },
			{ type: 'text', props: { text: ' ' } },
			{ type: 'emojiCode', props: { name: 'b' } },
			{ type: 'text', props: { text: ' ' } },
			{ type: 'mention', props: { username: 'c', host: null, acct: '@c' } },
		]);
	});

	test('passes the nest limit to the parser', () => {
		const nested = '**'.repeat(1) + '~~a~~' + '**'.repeat(1);
		expect(mfm.parse(nested, { nestLimit: 1 })[0].children?.[0].type).toBe('text');
		expect(mfm.parse(nested)[0].children?.[0].type).toBe('strike');
	});
});

describe('MFM parser imports', () => {
	// 画面のコードが解析器を直接読むと、このモジュールを通らなくなる。import 文だけで
	// なく import() (テンプレートリテラルを含む) や require、<パッケージ>/... の形も拾う。
	// eslint は import 文しか見ないので、ここでも確かめる。
	const roots = [join(__dirname, '../../src'), join(__dirname, '../../../frontend-embed/src'), join(__dirname, '../../../frontend-shared/js')];
	const files = (dir: string): string[] => readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
		const path = join(dir, entry.name);
		if (entry.isDirectory()) return files(path);
		return /\.(ts|tsx|js|vue)$/.test(entry.name) ? [path] : [];
	});

	const importersOf = (pkg: RegExp): string[] => roots.flatMap(files)
		.filter(path => new RegExp(`['"\`]${pkg.source}(/[^'"\`]*)?['"\`]`).test(readFileSync(path, 'utf8')))
		.map(path => path.replace(/\\/g, '/').replace(/^.*\/packages\//, ''))
		.sort();

	test('only the mfm module imports @elythia-network/mfm', () => {
		// 本家 2026.10.1 で MFM の描画が frontend-shared へ移ったので、入口も frontend-shared に置く。
		expect(importersOf(/@elythia-network\/mfm/)).toEqual([
			'frontend-shared/js/mfm.ts',
		]);
	});

	test('nothing imports mfm-js', () => {
		// mfm-js は #3515 で外した。入口のモジュールからも読まない。
		expect(importersOf(/mfm-js/)).toEqual([]);
	});
});
