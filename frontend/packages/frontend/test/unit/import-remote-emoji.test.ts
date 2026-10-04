/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, afterEach } from 'vitest';
import { bareEmojiName, hasLocalEmojiWithSameName } from '@/utility/import-remote-emoji.js';
import { customEmojisMap } from '@/custom-emojis.js';

/**
 * mk-go: リモート絵文字インポートの導線が使う名前の分解と同名判定 (#2903)。
 *
 * **この分解は元々 3 箇所に重複していた。** その重複のせいで、リアクション側の
 * 同名判定を「変数は作ったが条件式に配線し忘れる」形で出荷しかけた
 * (vue-tsc は通り、CI の eslint は --quiet なので未使用変数の warning も出ない)。
 * 1 箇所に集約したうえで、ここで固定する。
 */
describe('import-remote-emoji', () => {
	afterEach(() => {
		customEmojisMap.clear();
	});

	describe('bareEmojiName', () => {
		test('MkCustomEmoji が渡す裸の名前はそのまま', () => {
			expect(bareEmojiName('hoge')).toBe('hoge');
		});

		test('リアクションの name@host から host を落とす', () => {
			expect(bareEmojiName('hoge@example.com')).toBe('hoge');
		});

		// ローカルのリアクションは `@.` が付く形もありうる。
		test('ローカルマーク @. を落とす', () => {
			expect(bareEmojiName('hoge@.')).toBe('hoge');
		});

		// host 側にも @ を含む壊れた入力。**最後の @ で切る**ので name は保たれる。
		test('@ が複数あるときは最後で切る', () => {
			expect(bareEmojiName('ho@ge@example.com')).toBe('ho@ge');
		});

		test('空文字はそのまま', () => {
			expect(bareEmojiName('')).toBe('');
		});

		// 先頭が @ の壊れた入力。裸の名前が空になる。
		test('先頭が @ なら空になる', () => {
			expect(bareEmojiName('@example.com')).toBe('');
		});
	});

	describe('hasLocalEmojiWithSameName', () => {
		function seed(name: string) {
			customEmojisMap.set(name, {
				aliases: [], name, category: null, url: 'https://example.com/e.png',
				localOnly: false, isSensitive: false, roleIdsThatCanBeUsedThisEmojiAsReaction: [],
			});
		}

		test('同名のローカル絵文字があれば true', () => {
			seed('hoge');
			expect(hasLocalEmojiWithSameName('hoge')).toBe(true);
		});

		// **リアクションは name@host で来る。** host を落とさずに引くと常に false に
		// なり、同名でもインポート導線が出続ける。
		test('name@host でも裸の名前で判定する', () => {
			seed('hoge');
			expect(hasLocalEmojiWithSameName('hoge@example.com')).toBe(true);
		});

		test('無ければ false', () => {
			seed('other');
			expect(hasLocalEmojiWithSameName('hoge@example.com')).toBe(false);
		});

		// 空の名前でヒットさせない (Map に空キーがあっても導線を消さない)。
		test('裸の名前が空なら false', () => {
			seed('');
			expect(hasLocalEmojiWithSameName('')).toBe(false);
			expect(hasLocalEmojiWithSameName('@example.com')).toBe(false);
		});
	});
});
