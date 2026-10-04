/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, vi } from 'vitest';

// **実物の i18n は読み込まない。** locale 全体と `@@/js/config.js` (meta タグを
// 読む) を引き込むので、検証したい分岐ではなく環境の都合で落ちる。
vi.mock('@/i18n.js', () => ({
	i18n: {
		ts: {
			_emojiApplication: {
				errorRateLimited: 'RATE_LIMITED',
				errorQuotaExceededUnknown: 'GENERIC',
				errorPendingLimitExceededUnknown: 'PENDING_GENERIC',
				quotaPeriodDay: 'DAY',
				quotaPeriodWeek: 'WEEK',
				quotaPeriodMonth: 'MONTH',
			},
		},
		tsx: {
			_emojiApplication: {
				errorQuotaExceeded: (p: Record<string, unknown>) =>
					`period=${p.period} limit=${p.limit} retryAt=${p.retryAt}`,
				errorQuotaExceededNoRetryAt: (p: Record<string, unknown>) =>
					`no-retry period=${p.period} limit=${p.limit}`,
				errorPendingLimitExceeded: (p: Record<string, unknown>) => `pending used=${p.used} limit=${p.limit}`,
			},
		},
	},
}));

import { emojiApplicationQuotaText, emojiApplicationPendingLimitText } from '@/utility/emoji-application-quota.js';

const RETRY_AT = '2026-09-13T10:30:00.000Z';

function quotaError(info: unknown) {
	return { code: 'EMOJI_APPLICATION_QUOTA_EXCEEDED', info };
}

/**
 * mk-go: ロール別の期間上限 (#2958) を利用者向けの文面にする。
 *
 * **「申請しすぎです」に潰さない。** どの期間で・何件までで・いつ空くかが
 * 分からないと、待てば通るのか設定で塞がれているのかを区別できず叩き続ける。
 */
describe('emojiApplicationQuotaText', () => {
	test('期間・上限・再試行時刻をすべて文面に渡す', () => {
		const text = emojiApplicationQuotaText(quotaError({ period: 'day', limit: 3, retryAt: RETRY_AT }));
		expect(text).toContain('period=DAY');
		expect(text).toContain('limit=3');
		// 表示は閲覧者のタイムゾーンなので ISO のままではない。年だけを見る。
		expect(text).toContain('2026');
		expect(text).not.toContain(RETRY_AT);
	});

	test('期間ごとにラベルを変える', () => {
		const at = (period: string) => emojiApplicationQuotaText(quotaError({ period, limit: 1, retryAt: RETRY_AT }));
		expect(at('day')).toContain('period=DAY');
		expect(at('week')).toContain('period=WEEK');
		expect(at('month')).toContain('period=MONTH');
	});

	// **期間と件数が読めなければ汎用文へ落とす。** 「undefined 件まで」と
	// 出すくらいなら、上限に達したことだけ伝わる方がよい。
	test.each([
		['info ごと無い', undefined],
		['空', {}],
		['limit が数値でない', { period: 'day', limit: '3', retryAt: RETRY_AT }],
		['未知の期間', { period: 'year', limit: 3, retryAt: RETRY_AT }],
		['エラーそのものが null', null],
	])('%s のときは汎用文に落とす', (_label, info) => {
		expect(emojiApplicationQuotaText(info === null ? null : quotaError(info))).toBe('GENERIC');
	});

	// **時刻だけが無いのは審査待ちの上限も満杯のとき (#2977)。** サーバーが
	// 「予告できない」と判断して落としている。汎用文 (「しばらくしてから
	// もう一度」) に倒すと、**待っても通らないものを待たせる**ことになり、
	// 期間と件数という残っている情報も捨ててしまう。
	test.each([
		['retryAt が無い', { period: 'day', limit: 3 }],
		['retryAt が日付でない', { period: 'day', limit: 3, retryAt: 'soon' }],
		['retryAt が文字列でない', { period: 'day', limit: 3, retryAt: 1757760000000 }],
	])('%s のときは時刻に触れない文面を出す', (_label, info) => {
		const text = emojiApplicationQuotaText(quotaError(info));
		expect(text).toBe('no-retry period=DAY limit=3');
		expect(text).not.toBe('GENERIC');
	});

	// **`errorRateLimited` は使わない。** 「短時間に」と書くので、月次の窓で
	// 弾かれたとき (最大 30 日待つ) に事実と食い違う。
	test('退避文面に「短時間に」の文言を使わない', () => {
		expect(emojiApplicationQuotaText(quotaError({}))).not.toBe('RATE_LIMITED');
	});
});

/**
 * mk-go: 審査待ち件数の上限 (#2977)。
 *
 * **「しばらく待って」とは書かない。** 空くのはモデレーターが処理したときか
 * 自分で取り下げたときで、時間では解決しない。
 */
describe('emojiApplicationPendingLimitText', () => {
	// **`used` と `limit` の両方を出す。** 上限を後から下げると `used > limit`
	// になり、上限だけを見せると「何件取り下げればよいか」が伝わらない。
	test('審査待ちの件数と上限の両方を文面に渡す', () => {
		expect(emojiApplicationPendingLimitText({ info: { used: 5, limit: 3 } })).toBe('pending used=5 limit=3');
	});

	test.each([
		['info ごと無い', undefined],
		['空', {}],
		['limit が数値でない', { used: 5, limit: '3' }],
		['used が数値でない', { used: '5', limit: 3 }],
		['used が無い', { limit: 3 }],
	])('%s のときは件数を書かない', (_label, info) => {
		expect(emojiApplicationPendingLimitText({ info })).toBe('PENDING_GENERIC');
	});

	test('エラーそのものが null でも落ちない', () => {
		expect(emojiApplicationPendingLimitText(null)).toBe('PENDING_GENERIC');
	});

	// **期間の上限とは別の文面を出すこと。** 同じにすると「待てば通る」と
	// 誤解させ、実際には取り下げるまで通らない。
	test('期間の上限と同じ文面にしない', () => {
		const pending = emojiApplicationPendingLimitText({ info: { used: 5, limit: 3 } });
		const quota = emojiApplicationQuotaText({ info: { period: 'day', limit: 3, retryAt: RETRY_AT } });
		expect(pending).not.toBe(quota);
		expect(pending).not.toBe('GENERIC');
	});
});
