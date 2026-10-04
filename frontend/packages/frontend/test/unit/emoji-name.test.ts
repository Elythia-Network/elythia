/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect } from 'vitest';
import { isUsableEmojiName, EMOJI_NAME_MAX_LENGTH } from '@/utility/emoji-name.js';

// mk-go: リモート絵文字の名前がローカル絵文字として使えるか (#2998)。
//
// **backend の規則と揃っていること**を固定する。ずれると、UI が通したものが
// 400 で弾かれる (取り込めないのに理由が出ない) か、UI が弾いたものが実は
// 取り込めた、のどちらかになる。backend 側は `^[a-zA-Z0-9_]+$` と
// `emoji.name` varchar(128)。
describe('isUsableEmojiName', () => {
	test.each([
		['英数字', 'happy', true],
		['アンダースコア', 'blob_cat_2', true],
		['数字だけ', '123', true],
		// 本番の実測にあった 3 つの形。
		['記号だけ', '+_+', false],
		['@host が名前に残っている', 'ablobcatnodmeltcry@3.5mbps.net', false],
		['ハイフン', 'mikan_8-2', false],
		// 境界と、通しやすい誤り。
		['空文字', '', false],
		['空白を含む', 'foo bar', false],
		['ドット', 'foo.bar', false],
		['非 ASCII', 'にゃーん', false],
		['改行を末尾に付ける', 'happy\n', false],
	])('%s', (_label, name, want) => {
		expect(isUsableEmojiName(name)).toBe(want);
	});

	// **長さの境界はリテラルで書く。** `EMOJI_NAME_MAX_LENGTH` を使って両側を
	// 表すと、定数を何に変えても緑のまま通る (固定されるのは「その定数を境界に
	// 使っている」ことだけ)。ここで押さえたいのは **backend の `emoji.name`
	// varchar(128) と同じ値であること**なので、128 / 129 を直に書く。
	test('128 文字ちょうどは通る', () => {
		expect(isUsableEmojiName('a'.repeat(128))).toBe(true);
	});
	test('129 文字は弾く', () => {
		expect(isUsableEmojiName('a'.repeat(129))).toBe(false);
	});
	test('公開している定数も 128', () => {
		expect(EMOJI_NAME_MAX_LENGTH).toBe(128);
	});
});
