/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { i18n } from '@/i18n.js';
import { dateTimeFormat } from '@/utility/intl-const.js';

type QuotaInfo = {
	period?: unknown;
	limit?: unknown;
	retryAt?: unknown;
};

type PendingInfo = {
	used?: unknown;
	limit?: unknown;
};

/**
 * Renders the rolling-window rejection returned by `emoji-application/create`
 * (#2958). Callers must have matched `EMOJI_APPLICATION_QUOTA_EXCEEDED`.
 *
 * **どの期間で・何件までで・いつ空くかを出す。** 「申請しすぎです」だけだと、
 * 待てば通るのか設定で塞がれているのかが分からず、利用者は叩き続ける。
 */
export function emojiApplicationQuotaText(err: unknown): string {
	const info = (err as { info?: QuotaInfo } | null)?.info ?? {};
	const limit = typeof info.limit === 'number' ? info.limit : null;
	const period = quotaPeriodLabel(info.period);
	const retryAt = quotaRetryAtLabel(info.retryAt);
	// **期間と件数が読めなければ期間に依らない文面に落とす。**「undefined件まで」
	// と出すより、上限に達したことだけ伝わる方がよい。**`errorRateLimited` は
	// 使わない** — あちらは「短時間に」と書くので、月次の窓で弾かれたときに
	// 事実と食い違う (実際には最大 30 日待つ)。
	if (limit == null || period == null) {
		return i18n.ts._emojiApplication.errorQuotaExceededUnknown;
	}
	// **時刻が無いのは審査待ちの上限も満杯のとき (#2977)。** サーバーは
	// 「いつ空くか予告できない」と判断して時刻を落としている。ここで
	// 「しばらくしてからもう一度」と案内すると、**待っても通らないものを
	// 待たせる**ことになり、その空振りが 1 時間あたりの制限を食う。
	if (retryAt == null) {
		return i18n.tsx._emojiApplication.errorQuotaExceededNoRetryAt({ period, limit });
	}
	return i18n.tsx._emojiApplication.errorQuotaExceeded({ period, limit, retryAt });
}

function quotaPeriodLabel(period: unknown): string | null {
	switch (period) {
		case 'day': return i18n.ts._emojiApplication.quotaPeriodDay;
		case 'week': return i18n.ts._emojiApplication.quotaPeriodWeek;
		case 'month': return i18n.ts._emojiApplication.quotaPeriodMonth;
		// 期間の種類が増えたときに嘘のラベルを出さない。
		default: return null;
	}
}

function quotaRetryAtLabel(retryAt: unknown): string | null {
	if (typeof retryAt !== 'string') return null;
	const at = new Date(retryAt);
	if (Number.isNaN(at.getTime())) return null;
	return dateTimeFormat.format(at);
}

/**
 * Renders the awaiting-review rejection (#2977). Callers must have matched
 * `EMOJI_APPLICATION_PENDING_LIMIT_EXCEEDED`.
 *
 * **「しばらく待って」とは書かない。** 空くのはモデレーターが処理したときか、
 * 自分で取り下げたとき。時間で解決すると書くと、待ち続けることになる。
 */
export function emojiApplicationPendingLimitText(err: unknown): string {
	const info = (err as { info?: PendingInfo } | null)?.info ?? {};
	if (typeof info.used !== 'number' || typeof info.limit !== 'number') {
		// **件数が読めなければ件数を書かない。** 退避先も本文と同じく必要条件
		// だけを述べる (「〜まで新しく申請できません」)。**「取り下げれば出せる」
		// とは書かない** — それが成り立つかはサーバー側の評価順序次第で、
		// 一度その約束を書いて誤りになっている (mk.18b)。
		return i18n.ts._emojiApplication.errorPendingLimitExceededUnknown;
	}
	// **`used` も出す。** 上限を後から下げると `used > limit` になり、上限の
	// 件数だけを見せると「何件取り下げればよいか」が伝わらない (7 件あって
	// 上限 3 なら 5 件取り下げる必要がある)。
	return i18n.tsx._emojiApplication.errorPendingLimitExceeded({ used: info.used, limit: info.limit });
}
