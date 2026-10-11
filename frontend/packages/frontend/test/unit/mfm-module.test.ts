/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, test } from 'vitest';
import * as mfm from '@/utility/mfm.js';

describe('parse', () => {
	test('does not pass options other than nestLimit to mfm-js', () => {
		// パッチのテスト用の口 (workLimit) を画面のコードから通さない。
		expect(mfm.parse('**a**', { workLimit: 0 } as never)[0].type).toBe('bold');
	});

	test('parses ordinary input as mfm-js does', () => {
		expect(mfm.parse('**a** :b: @c')).toEqual([
			{ type: 'bold', children: [{ type: 'text', props: { text: 'a' } }] },
			{ type: 'text', props: { text: ' ' } },
			{ type: 'emojiCode', props: { name: 'b' } },
			{ type: 'text', props: { text: ' ' } },
			{ type: 'mention', props: { username: 'c', host: null, acct: '@c' } },
		]);
	});

	test('passes the nest limit to mfm-js', () => {
		const nested = '**'.repeat(1) + '~~a~~' + '**'.repeat(1);
		expect(mfm.parse(nested, { nestLimit: 1 })[0].children?.[0].type).toBe('text');
		expect(mfm.parse(nested)[0].children?.[0].type).toBe('strike');
	});
});

describe('mfm-js imports', () => {
	// 画面のコードが mfm-js を直接読むと、このモジュールを通らなくなる。import 文だけで
	// なく import() (テンプレートリテラルを含む) や require、mfm-js/... の形も拾う。
	// eslint は import 文しか見ないので、ここでも確かめる。
	const roots = [join(__dirname, '../../src'), join(__dirname, '../../../frontend-embed/src')];
	const files = (dir: string): string[] => readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
		const path = join(dir, entry.name);
		if (entry.isDirectory()) return files(path);
		return /\.(ts|tsx|js|vue)$/.test(entry.name) ? [path] : [];
	});

	test('only the mfm module imports mfm-js', () => {
		const importers = roots.flatMap(files)
			.filter(path => /['"`]mfm-js(\/[^'"`]*)?['"`]/.test(readFileSync(path, 'utf8')))
			.map(path => path.replace(/\\/g, '/').replace(/^.*\/packages\//, ''));
		expect(importers.sort()).toEqual([
			'frontend-embed/src/utility/mfm.ts',
			'frontend/src/utility/mfm.ts',
		]);
	});
});
