/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';
import { minimatch } from 'minimatch';
import { afterEach, beforeAll, describe, expect, test, vi } from 'vitest';
import pluginCreateSearchIndex, { collectFileMarkers, MarkerIdAssigner } from '../../lib/vite-plugin-create-search-index.js';
import { searchIndexes } from '../../vite.config.js';

const root = path.resolve(__dirname, '../..');

// ビルドと同じ対象を、vite.config.ts の searchIndexes から列挙する。
const files = searchIndexes.flatMap(({ targetFilePaths }) => targetFilePaths.flatMap(pattern => {
	const dir = path.dirname(pattern);
	return readdirSync(path.join(root, dir))
		.map(name => path.join(dir, name))
		.filter(file => minimatch(file, pattern));
}))
	.map(file => ({ file, code: readFileSync(path.join(root, file), 'utf8') }))
	.filter(({ code }) => /<SearchMarker[\s>]/.test(code));

// 本家 (2026.10.0) と同じ形のまま残しているもの。ファイルごとに、出てよい
// 「Duplicate SearchLabel」の警告の数を書く。1 つの SearchMarker の中に
// SearchLabel が複数あり、2 つ目以降が無視されて検索に出ないもの。直したら
// 数が合わなくなって落ちるので、ここから外す。
const knownDuplicateLabels: Record<string, number> = {
	[path.join('src/pages/admin', 'performance.vue')]: 3,
};

describe('settings search index', () => {
	// 解析器は失敗しても、ビルドのログに出すだけで止まらない (#3548)。評価に
	// 失敗するとファイル全体を捨て、ラベルが文字列にならないときや重複したときは
	// 項目を残したまま言うだけなので、ログに出たものを全部失敗として扱う。
	beforeAll(() => {
		// ロガーはプラグインを作るときにだけ有効になる。
		pluginCreateSearchIndex({ targetFilePaths: [], mainVirtualModule: 'search-index:test', modulesToHmrOnUpdate: [] });
	});

	afterEach(() => {
		vi.restoreAllMocks();
	});

	test('finds pages to check', () => {
		expect(files.some(({ file }) => file === path.join('src/pages/settings', 'privacy.vue'))).toBe(true);
		expect(files.length).toBeGreaterThan(20);
	});

	// 対象から外れたファイルの項目が残ると、後で同じ名前のファイルの重複を黙って許す。
	test('has no stale known duplicates', () => {
		const checked = new Set(files.map(({ file }) => file));
		expect(Object.keys(knownDuplicateLabels).filter(file => !checked.has(file))).toEqual([]);
	});

	test.each(files)('indexes every SearchMarker in $file', ({ file, code }) => {
		const logged: string[] = [];
		vi.spyOn(console, 'warn').mockImplementation((msg: unknown) => { logged.push(String(msg)); });
		vi.spyOn(console, 'error').mockImplementation((msg: unknown) => { logged.push(String(msg)); });

		const id = path.join(root, file);
		const transformed = new MarkerIdAssigner().processFile(id, code)?.code;
		const items = collectFileMarkers(id, transformed);

		const allowed = knownDuplicateLabels[file] ?? 0;
		expect(logged).toHaveLength(allowed);
		expect(logged.filter(msg => !msg.includes('Duplicate SearchLabel found'))).toEqual([]);
		// コメントの中のタグは AST に出ないので数えない。
		const markerTags = code.replace(/<!--[\s\S]*?-->/g, '').match(/<SearchMarker[\s>]/g)?.length ?? 0;
		expect(items).toHaveLength(markerTags);
		expect(items.filter(item => item.label === '').map(item => item.id)).toEqual([]);
	});
});
