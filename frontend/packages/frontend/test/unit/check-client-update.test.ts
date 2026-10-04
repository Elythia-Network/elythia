/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import { mkGoChangelogUrl, resolveClientUpdate } from '@/utility/check-client-update.js';

// 引数を毎回 4 つ書くと、どれを動かした test なのかが読めなくなる。
const check = (over: Partial<Parameters<typeof resolveClientUpdate>[0]> = {}) => resolveClientUpdate({
	misskeyVersion: '2026.9.0',
	mkGoVersion: '1.4.0',
	lastMisskeyVersion: '2026.9.0',
	lastMkGoVersion: '1.3.0',
	...over,
});

describe('resolveClientUpdate', () => {
	test('shows the dialog when the mk-go version went up', () => {
		expect(check()).toMatchObject({ updated: true, isMkGo: true, version: '1.4.0' });
	});

	// **同じ版で出してはいけない。** boot は毎回通るので、ここが緩いと
	// リロードのたびにダイアログが出る。
	test('stays quiet when the mk-go version is unchanged', () => {
		expect(check({ lastMkGoVersion: '1.4.0' }).updated).toBe(false);
	});

	// 初回訪問 / localStorage を消した直後。「更新されました」は嘘になる。
	// updated 以外も固定する。ここを緩くすると `isMkGo: true` を
	// `isMkGo: args.lastMkGoVersion != null` に変えても全部通ってしまう。
	test('stays quiet on the first visit', () => {
		expect(check({ lastMkGoVersion: null })).toMatchObject({
			updated: false, isMkGo: true, version: '1.4.0',
		});
	});

	test('stays quiet on a downgrade', () => {
		expect(check({ mkGoVersion: '1.2.0' }).updated).toBe(false);
	});

	// compareVersions は解釈できない文字列で throw する。boot ごと落とさない。
	test('stays quiet when a version string cannot be parsed', () => {
		expect(check({ lastMkGoVersion: 'なにこれ' }).updated).toBe(false);
		expect(check({ mkGoVersion: 'なにこれ' }).updated).toBe(false);
	});

	// **純正 backend では upstream の挙動をそのまま残す。**
	describe('without mkGoVersion (vanilla Misskey backend)', () => {
		test('falls back to the Misskey version', () => {
			expect(check({ mkGoVersion: null, lastMisskeyVersion: '2026.8.0' }))
				.toMatchObject({ updated: true, isMkGo: false, version: '2026.9.0' });
		});

		// mkGoVersion を載せていない古い mk-go や、meta を差し替える前段が
		// 挟まっている構成では空文字が来うる。載っていないのと同じ扱い。
		test('treats an empty mkGoVersion as absent', () => {
			expect(check({ mkGoVersion: '', lastMisskeyVersion: '2026.8.0' }).isMkGo).toBe(false);
		});

		// **mk-go 側が上がっていても引きずられない。** 純正に繋いだのに
		// mk-go の版で出すと、リンク先の CHANGELOG と実物が食い違う。
		test('ignores a stale lastMkGoVersion', () => {
			expect(check({ mkGoVersion: null, lastMkGoVersion: '0.1.0' }).updated).toBe(false);
		});
	});

	// **版体系を混ぜない。** 同じキーに入れると compareVersions('2026.9.0', '1.3.0')
	// が 1 を返し、backend を差し替えただけで誤検知する。
	test('does not compare the two version lines against each other', () => {
		expect(check({ lastMkGoVersion: '2026.9.0' }).updated).toBe(false);
	});
});

// **ここが一番壊れたときに痛い。** 保存し損ねると lastMkGoVersion が更新されず、
// ログイン中の全利用者にページ遷移のたびにダイアログが出続ける。判断を純関数へ
// 寄せてあるのは、呼び出し側に条件を置くと反転させても全検査が緑になるため
// (敵対的レビューで実測された)。
describe('resolveClientUpdate persistence', () => {
	test('records the new mk-go version so the dialog is not shown again', () => {
		expect(check().persist).toContainEqual(['lastMkGoVersion', '1.4.0']);
	});

	test('writes nothing when neither version changed', () => {
		expect(check({ lastMkGoVersion: '1.4.0' }).persist).toEqual([]);
	});

	// **両方を揃える。** 判定に使った側だけ書くと、backend を差し替えたときに
	// 片側が数世代前のまま残り、戻した瞬間に誤検知する。
	// 順序も固定する — 機能上は依存しないが、出力が決定的であることを
	// 崩さないため (並べ替えが要るなら、まずこの test を直すことになる)。
	test('records both version lines when both moved', () => {
		expect(check({ lastMisskeyVersion: '2026.8.0' }).persist).toEqual([
			['lastVersion', '2026.9.0'],
			['lastMkGoVersion', '1.4.0'],
		]);
	});

	test('keeps the Misskey version fresh even while mk-go decides', () => {
		expect(check({ lastMisskeyVersion: '2026.8.0' }).persist)
			.toContainEqual(['lastVersion', '2026.9.0']);
	});

	// 純正 backend では mk-go 側のキーを作らない。作ると次に mk-go へ繋いだとき、
	// 実際には見ていない版を「もう見た」ことにしてしまう。
	test('does not invent a mk-go key on a vanilla backend', () => {
		const persist = check({ mkGoVersion: null, lastMisskeyVersion: '2026.8.0' }).persist;
		expect(persist.map(([key]) => key)).toEqual(['lastVersion']);
	});

	test('does not store an empty mk-go version', () => {
		const persist = check({ mkGoVersion: '', lastMisskeyVersion: '2026.8.0' }).persist;
		expect(persist.map(([key]) => key)).toEqual(['lastVersion']);
	});

	// ダウングレードでもダイアログは出さないが、**保存はする** — しないと
	// 下げた版に留まるあいだ毎回 compareVersions を走らせ続けることになる。
	test('records a downgrade even though no dialog is shown', () => {
		const result = check({ mkGoVersion: '1.2.0' });
		expect(result.updated).toBe(false);
		expect(result.persist).toContainEqual(['lastMkGoVersion', '1.2.0']);
	});
});

describe('mkGoChangelogUrl', () => {
	test('builds the anchor from the version', () => {
		expect(mkGoChangelogUrl('1.3.0'))
			.toBe('https://github.com/shiroha-a/mk/blob/main/CHANGELOG.md#130');
	});

	test('strips every dot, not just the first', () => {
		expect(mkGoChangelogUrl('1.10.2')).toMatch(/#1102$/);
	});
});
