/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, vi } from 'vitest';

vi.mock('@/i18n.js', () => ({
	i18n: {
		ts: {
			_emojiApplication: {
				quotaPeriodDay: 'DAY',
				quotaPeriodWeek: 'WEEK',
				quotaPeriodMonth: 'MONTH',
			},
		},
		tsx: {
			_emojiApplication: {
				quotaUsage: (p: Record<string, unknown>) => `${p.used}/${p.limit}`,
				quotaUsageUnlimited: (p: Record<string, unknown>) => `${p.used}(none)`,
			},
		},
	},
}));

import { canLoadMoreUserApplications, canResetQuota, isValidResetReason, quotaIsFull, quotaPeriodLabel, quotaUsageLabel, userApplicationNextCursor } from '@/utility/emoji-application-user.js';

const win = (over: Partial<{ period: string; used: number; limit: number; unlimited: boolean }> = {}) => ({
	period: 'day', used: 0, limit: 0, unlimited: true, ...over,
});

/**
 * mk-go: ユーザーモデレーション画面の申請履歴 (#2961)。
 */
describe('quotaPeriodLabel', () => {
	test('既知の期間はラベルにする', () => {
		expect(quotaPeriodLabel('day')).toBe('DAY');
		expect(quotaPeriodLabel('week')).toBe('WEEK');
		expect(quotaPeriodLabel('month')).toBe('MONTH');
	});

	// **未知の期間でラベルを捏造しない。** サーバー側に窓が増えたとき、既存の
	// ラベルを当てると「1日」と出ているのに実際は別の期間、という案内になる。
	test('未知の期間は生のまま出す', () => {
		expect(quotaPeriodLabel('year')).toBe('year');
		expect(quotaPeriodLabel('')).toBe('');
	});
});

describe('quotaIsFull', () => {
	// **`>=` で見る。** `>` だと、ちょうど上限に達した状態を「まだ出せる」と
	// 描く (作成側は used >= Max で弾く)。
	test('上限ちょうどで満杯', () => {
		expect(quotaIsFull(win({ used: 5, limit: 5, unlimited: false }))).toBe(true);
		expect(quotaIsFull(win({ used: 4, limit: 5, unlimited: false }))).toBe(false);
		expect(quotaIsFull(win({ used: 6, limit: 5, unlimited: false }))).toBe(true);
	});

	// 無制限の窓は何件出していても満杯にならない。
	test('無制限は満杯にならない', () => {
		expect(quotaIsFull(win({ used: 999, limit: 0, unlimited: true }))).toBe(false);
		// サーバーが unlimited を落としても、limit 0 は上限なしとして扱う。
		expect(quotaIsFull(win({ used: 999, limit: 0, unlimited: false }))).toBe(false);
	});
});

describe('quotaUsageLabel', () => {
	test('上限があれば used / limit', () => {
		expect(quotaUsageLabel(win({ used: 3, limit: 5, unlimited: false }))).toBe('3/5');
	});

	// **無制限を「3 / 0」と描かない。** 0 を分母にすると枠が尽きているように
	// 見える。
	test('無制限は分母を出さない', () => {
		expect(quotaUsageLabel(win({ used: 3, limit: 0, unlimited: true }))).toBe('3(none)');
		expect(quotaUsageLabel(win({ used: 3, limit: 0, unlimited: false }))).toBe('3(none)');
	});
});

// **審査待ちの上限も同じ関数で描く (レビュー L-6)。** 期間の窓と形が同じなので
// 取り違えても型では落ちない。0 を分母にしない・上限ちょうどで満杯、を固定する。
describe('審査待ちの上限', () => {
	const pending = (used: number, limit: number, unlimited: boolean) => ({ used, limit, unlimited });

	test('上限があれば used / limit', () => {
		expect(quotaUsageLabel(pending(2, 3, false))).toBe('2/3');
		expect(quotaIsFull(pending(2, 3, false))).toBe(false);
		expect(quotaIsFull(pending(3, 3, false))).toBe(true);
	});

	test('上限なしは分母を出さず満杯にもならない', () => {
		expect(quotaUsageLabel(pending(5, 0, true))).toBe('5(none)');
		expect(quotaIsFull(pending(5, 0, true))).toBe(false);
	});
});

describe('userApplicationNextCursor', () => {
	// **末尾を採る。** 先頭を渡すと同じページを永久に読み直す。
	test('末尾の id を返す', () => {
		expect(userApplicationNextCursor([{ id: 'c' }, { id: 'b' }, { id: 'a' }])).toBe('a');
	});

	test('空なら undefined', () => {
		expect(userApplicationNextCursor([])).toBeUndefined();
	});
});

describe('canLoadMoreUserApplications', () => {
	// **満杯のページが返った時だけ続きがある。** 総数と突き合わせると、status で
	// 絞っているときに総数 (全ステータス) と食い違って空の追い読みが出る。
	test('ページが満杯なら続きがある', () => {
		expect(canLoadMoreUserApplications(30, 30)).toBe(true);
		expect(canLoadMoreUserApplications(29, 30)).toBe(false);
		expect(canLoadMoreUserApplications(0, 30)).toBe(false);
	});
});

// mk-go: 申請枠の手動リセット (#2962)。
describe('canResetQuota', () => {
	const w = (limit: number, unlimited: boolean) => ({ period: 'day', used: 0, limit, unlimited });

	// **すべて無制限なら出さない。** 戻す枠が無いので、押しても何も変わらない
	// 操作を「効いたように見える」形で出すことになる。
	test('すべて無制限なら false', () => {
		expect(canResetQuota([w(0, true), w(0, true)])).toBe(false);
		expect(canResetQuota([])).toBe(false);
	});

	test('1つでも上限があれば true', () => {
		expect(canResetQuota([w(0, true), w(5, false), w(0, true)])).toBe(true);
	});

	// サーバーが unlimited を落としても limit 0 は上限なしとして扱う。
	test('limit 0 は上限なし扱い', () => {
		expect(canResetQuota([w(0, false)])).toBe(false);
	});
});

describe('isValidResetReason', () => {
	// **空白だけを通さない。** 監査ログに残る唯一の文脈。
	test('空や空白だけは false', () => {
		expect(isValidResetReason('')).toBe(false);
		expect(isValidResetReason('   ')).toBe(false);
		expect(isValidResetReason('\t\n')).toBe(false);
	});

	test('中身があれば true', () => {
		expect(isValidResetReason('再申請してもらうため')).toBe(true);
		expect(isValidResetReason('  x  ')).toBe(true);
	});
});
