/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import { MAX_USERNAME_LENGTH, MIN_USERNAME_LENGTH, resolveLocalUsernameState, resolveMinimumUsernameLength } from '@/utility/local-username.js';

// mk-go: 新規登録の username の最小文字数 (#3015)。
//
// **サーバー側 (`internal/core/signup` の `minimumUsernameLength`) と同じ
// 丸め方であることが要点。** ここだけが別の判定をすると、入力中は「使えます」と
// 見えて送信で弾かれる (逆もある)。

describe('resolveMinimumUsernameLength', () => {
	test('純正 backend (field が無い) では 1', () => {
		expect(resolveMinimumUsernameLength({})).toBe(1);
	});

	test('meta そのものが無くても落ちない', () => {
		expect(resolveMinimumUsernameLength(null)).toBe(1);
		expect(resolveMinimumUsernameLength(undefined)).toBe(1);
	});

	test('設定値をそのまま返す', () => {
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: 5 })).toBe(5);
	});

	test('列が未設定 (0) や負値は 1 に倒す', () => {
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: 0 })).toBe(1);
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: -5 })).toBe(1);
	});

	// **上限を丸めないと、どの username も 20 文字までなので入力が全滅する。**
	test('20 を超える値は 20 に丸める', () => {
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: 21 })).toBe(20);
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: 999 })).toBe(20);
	});

	test('境界はそのまま通る', () => {
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: MIN_USERNAME_LENGTH })).toBe(MIN_USERNAME_LENGTH);
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: MAX_USERNAME_LENGTH })).toBe(MAX_USERNAME_LENGTH);
	});

	// **数値以外を弾かないと NaN が比較に混ざり、どの長さでも false になる。**
	// 途中の前段 (nginx や別実装の backend) が meta を差し替える構成がありうる。
	test('数値でない値や NaN は 1 に倒す', () => {
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: '5' })).toBe(1);
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: NaN })).toBe(1);
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: Infinity })).toBe(1);
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: null })).toBe(1);
	});

	test('小数は切り捨てる', () => {
		expect(resolveMinimumUsernameLength({ minimumUsernameLength: 5.9 })).toBe(5);
	});
});

describe('resolveLocalUsernameState', () => {
	// **サーバーが返す USERNAME_TOO_SHORT と同じ境界であること。**
	test('最小文字数の境界', () => {
		expect(resolveLocalUsernameState('abcd', 5)).toBe('min-range');
		expect(resolveLocalUsernameState('abcde', 5)).toBe(null);
		expect(resolveLocalUsernameState('abcdef', 5)).toBe(null);
	});

	// 既定 (1) では従来どおり 1 文字でも通る。**既存インスタンスを壊さない。**
	test('既定 (1) では 1 文字でも通る', () => {
		expect(resolveLocalUsernameState('a', MIN_USERNAME_LENGTH)).toBe(null);
	});

	test('20 文字を超えると max-range', () => {
		expect(resolveLocalUsernameState('a'.repeat(MAX_USERNAME_LENGTH), MIN_USERNAME_LENGTH)).toBe(null);
		expect(resolveLocalUsernameState('a'.repeat(MAX_USERNAME_LENGTH + 1), MIN_USERNAME_LENGTH)).toBe('max-range');
	});

	// **文字種を先に見る。** 記号入りの短い名前に「n 文字以上にしてください」を
	// 出すと、長くしても通らない案内になる。
	test('文字種の違反は最小文字数より先に出る', () => {
		expect(resolveLocalUsernameState('a-b', 10)).toBe('invalid-format');
		expect(resolveLocalUsernameState('日本語', 10)).toBe('invalid-format');
	});
});
