/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, describe, expect, test } from 'vitest';
import { localAlternativeReaction } from '@/utility/reaction-alternative.js';
import { customEmojisMap } from '@/custom-emojis.js';

/**
 * mk-go: リモートのリアクションにローカルの同名絵文字で相乗りする (#2697)。
 *
 * **返すのは `:foo@.:` (ローカルホストマーク付き)。** `:foo:` でも backend は
 * ローカルの絵文字に解決するが、`myReaction` は `:foo@.:` で返るので、送る
 * 文字列を揃えないと「既に自分が押しているか」の比較が外れ、押し直しが
 * 「取り消し」ではなく「付け替え」になる。
 */
describe('localAlternativeReaction', () => {
	afterEach(() => {
		customEmojisMap.clear();
	});

	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	const addLocal = (name: string) => customEmojisMap.set(name, { name } as any);

	test('ローカルに同名があるリモートのリアクションは差し替える', () => {
		addLocal('hoge');
		expect(localAlternativeReaction(':hoge@example.com:')).toBe(':hoge@.:');
	});

	test('ローカルに同名が無ければ差し替えない', () => {
		expect(localAlternativeReaction(':hoge@example.com:')).toBeNull();
	});

	test('別の名前があっても差し替えない', () => {
		addLocal('fuga');
		expect(localAlternativeReaction(':hoge@example.com:')).toBeNull();
	});

	test('既にローカルのリアクションは差し替えない', () => {
		addLocal('hoge');
		expect(localAlternativeReaction(':hoge@.:')).toBeNull();
	});

	test('Unicode 絵文字は差し替えない', () => {
		addLocal('👍');
		expect(localAlternativeReaction('👍')).toBeNull();
	});

	test('host にドットが複数あっても名前だけを見る', () => {
		addLocal('hoge');
		expect(localAlternativeReaction(':hoge@sub.example.com:')).toBe(':hoge@.:');
	});

	test('名前に @ を含んでも最後の @ で切る', () => {
		// `getEmojiNameFromReaction` は `@.` を落とすだけなので、名前と host の
		// 分解は `bareEmojiName` (最後の `@` で切る) に任せている。
		// backend の `emojiNamePattern` が `@` を許さないので**実際には来ない**。
		// 防御的な実装として固定しておく。
		addLocal('ho@ge');
		expect(localAlternativeReaction(':ho@ge@example.com:')).toBe(':ho@ge@.:');
	});

	test('host マークの無い :name: もローカルに寄せる', () => {
		// `@.` を持たない形は `entity.packReactions` / `use-note-capture` が
		// 正規化するので**実際には来ない**。防御的な実装として固定しておく。
		addLocal('hoge');
		expect(localAlternativeReaction(':hoge:')).toBe(':hoge@.:');
	});

	test('名前が空なら差し替えない', () => {
		addLocal('hoge');
		expect(localAlternativeReaction('::')).toBeNull();
		expect(localAlternativeReaction(':@example.com:')).toBeNull();
	});
});
