/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect } from 'vitest';
import { getCustomEmojiImagePath, isCustomEmojiImagePath } from '@@/js/emoji-name.js';

// MkCustomEmoji / EmCustomEmoji は、この判定に通った URL だけをメディアプロキシに
// 通さずにそのまま読み込む。`getCustomEmojiImagePath` が作る形だけを通し、
// パストラバーサルなど、それ以外の形は通さない。
describe('isCustomEmojiImagePath', () => {
	test.each([
		['ローカルの絵文字', getCustomEmojiImagePath('blob_cat', null), true],
		['リモートの絵文字', getCustomEmojiImagePath('blob+cat-2', 'remote.example'), true],
		['ドットを含む名前', '/emoji/foo.bar.webp', true],
		['上の階層', '/emoji/../x', false],
		['上の階層で .webp で終わる', '/emoji/../a/x.webp', false],
		['クエリ', '/emoji/x?.webp', false],
		['フラグメント', '/emoji/x#.webp', false],
		['符号化したスラッシュ', '/emoji/..%2fx.webp', false],
		['拡張子が違う', '/emoji/blob_cat.png', false],
		['末尾に改行', '/emoji/blob_cat.webp\n', false],
		['別の階層', '/files/blob_cat.webp', false],
		['絶対 URL', 'https://remote.example/emoji/blob_cat.webp', false],
		['プロトコル相対', '//remote.example/emoji/blob_cat.webp', false],
	])('%s', (_label, url, want) => {
		expect(isCustomEmojiImagePath(url)).toBe(want);
	});
});
