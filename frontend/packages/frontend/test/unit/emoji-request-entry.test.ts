/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, vi, beforeEach } from 'vitest';

// `$i` は module 評価時に localStorage から作られるので、差し替えられるよう
// mock にする。**実物を読み込まない** — local-storage / reactive を引き込むと、
// 検証したい分岐ではなく環境の都合で落ちる。
const state: { i: unknown } = { i: null };
vi.mock('@/i.js', () => ({
	get $i() { return state.i; },
}));

const { canShowEmojiRequestEntry } = await import('@/utility/emoji-request-entry.js');

type Account = {
	isModerator?: boolean;
	policies: Record<string, unknown>;
};

function signin(account: Account | null): void {
	state.i = account;
}

describe('canShowEmojiRequestEntry', () => {
	beforeEach(() => signin(null));

	test('未ログインでは出さない', () => {
		expect(canShowEmojiRequestEntry()).toBe(false);
	});

	test('policy を持っていれば出す', () => {
		signin({ policies: { canRequestCustomEmojis: true } });
		expect(canShowEmojiRequestEntry()).toBe(true);
	});

	test('policy が無ければ出さない', () => {
		signin({ policies: { canRequestCustomEmojis: false } });
		expect(canShowEmojiRequestEntry()).toBe(false);

		signin({ policies: {} });
		expect(canShowEmojiRequestEntry()).toBe(false);
	});

	// **モデレーターには出さない。** 直接登録できるので、申請ページへ送っても
	// 遠回りになるだけ。mk-go の `IsModerator` は管理者と root にも true を
	// 返すので、この 1 つで管理者も除外される。
	test('モデレーターには出さない', () => {
		signin({ isModerator: true, policies: { canRequestCustomEmojis: true } });
		expect(canShowEmojiRequestEntry()).toBe(false);
	});

	// **管理権限を優先する。** 両方を持つ状態でも管理導線だけを出す。
	test('canManageCustomEmojis を持つ人には出さない', () => {
		signin({ policies: { canRequestCustomEmojis: true, canManageCustomEmojis: true } });
		expect(canShowEmojiRequestEntry()).toBe(false);
	});

	// **真偽値でなければ出さない (fail-closed)。** policy は admin API から
	// 型検証なしで書けるので、文字列が届きうる。
	test('bool 以外は出さない', () => {
		signin({ policies: { canRequestCustomEmojis: 'true' } });
		expect(canShowEmojiRequestEntry()).toBe(false);
	});
});
