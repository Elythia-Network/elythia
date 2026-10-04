/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { i18n } from '@/i18n.js';

/**
 * One rolling window as returned by `admin/emoji-application/user-summary` (#2961).
 */
export type QuotaWindowView = {
	period: string;
	used: number;
	limit: number;
	unlimited: boolean;
	retryAt?: string | null;
};

/**
 * The awaiting-review cap as returned by `user-summary` (#2961 / #2977).
 *
 * **窓とは別に出す。** 期間の窓に空きがあっても、これが満杯なら申請は 400 で
 * 弾かれる。出さないと画面は「1日: 2 / 10 (空きあり)」と描き、**実際には
 * 出せない人を出せると案内する**。
 */
export type PendingLimitView = {
	used: number;
	limit: number;
	unlimited: boolean;
};

/**
 * Renders the period label (#2961).
 *
 * **未知の期間でラベルを捏造しない。** サーバー側に窓が増えたとき、既存の
 * ラベルのどれかを当てると「1日」と出ているのに実際は別の期間、という誤った
 * 案内になる。生の値を出しておけば、少なくとも嘘にはならない。
 */
export function quotaPeriodLabel(period: string): string {
	switch (period) {
		case 'day': return i18n.ts._emojiApplication.quotaPeriodDay;
		case 'week': return i18n.ts._emojiApplication.quotaPeriodWeek;
		case 'month': return i18n.ts._emojiApplication.quotaPeriodMonth;
		default: return period;
	}
}

/**
 * Reports whether the window is full (#2961).
 *
 * **`>=` で見る。** `>` にすると、ちょうど上限に達した状態を「まだ出せる」と
 * 描いてしまう (作成側は `used >= Max` で弾く)。
 */
export function quotaIsFull(w: QuotaWindowView | PendingLimitView): boolean {
	if (w.unlimited || w.limit <= 0) return false;
	return w.used >= w.limit;
}

/**
 * Renders "used / limit" for one window (#2961).
 *
 * **無制限を「3 / 0」と描かない。** 0 を分母にすると枠が尽きているように
 * 見える。上限が無いことと、使った件数は別々に伝える。
 */
export function quotaUsageLabel(w: QuotaWindowView | PendingLimitView): string {
	if (w.unlimited || w.limit <= 0) {
		return i18n.tsx._emojiApplication.quotaUsageUnlimited({ used: w.used });
	}
	return i18n.tsx._emojiApplication.quotaUsage({ used: w.used, limit: w.limit });
}

/**
 * Picks the cursor for the next page (#2961).
 *
 * **末尾を採る。** 先頭を渡すと同じページを永久に読み直す (id の降順)。
 */
export function userApplicationNextCursor(items: readonly { id: string }[]): string | undefined {
	if (items.length === 0) return undefined;
	return items[items.length - 1].id;
}

/**
 * Reports whether more rows can be loaded (#2961).
 *
 * **「返ってきた件数が limit と同じ」で判断する。** 集計の総数と突き合わせると、
 * status で絞っているときに総数 (全ステータス) と食い違う。
 */
export function canLoadMoreUserApplications(lastPageSize: number, limit: number): boolean {
	return lastPageSize >= limit && lastPageSize > 0;
}

/**
 * The last manual quota reset as returned by `user-summary` (#2962).
 */
export type QuotaResetView = {
	at: string;
	byId: string;
	reason: string;
};

/**
 * Reports whether the manual reset is worth offering (#2962).
 *
 * **すべての期間上限が無制限ならボタンを出さない。** 戻す枠が無いので、押しても
 * 何も変わらない操作を「効いたように見える」形で提供することになる。監査ログ
 * だけが増える。
 *
 * **審査待ちの上限は数えない。** リセットはそちらに効かない (戻しても申請は
 * 審査待ちのまま残る) ので、それだけを理由にボタンを出すと期待を裏切る。
 */
export function canResetQuota(windows: readonly QuotaWindowView[]): boolean {
	return windows.some(w => !w.unlimited && w.limit > 0);
}

/**
 * Validates the reason before sending the reset (#2962).
 *
 * **空白だけを通さない。** 監査ログに残る唯一の文脈なので、server も同じ判定で
 * 400 を返す。手前で弾くのは往復を減らすためで、判定そのものを client に
 * 委ねているわけではない。
 */
export function isValidResetReason(reason: string): boolean {
	return reason.trim().length > 0;
}
