/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { i18n } from '@/i18n.js';
import { getProxiedImageUrl } from '@/utility/media-proxy.js';

export type RelatedCounts = {
	total: number;
	pending: number;
	approved: number;
	rejected: number;
	canceled: number;
};

export type RelatedItem = {
	id: string;
	url: string | null;
	remoteHost?: string;
};

export type RelatedStatus = 'pending' | 'approved' | 'rejected' | 'canceled';

/**
 * Renders the label for one match reason (#2960).
 *
 * **未知の条件でラベルを捏造しない。** サーバー側に条件が増えたとき、既存の
 * ラベルのどれかを当てると「名前が同じ」と出ているのに実際は別の理由、という
 * 誤った案内になる。生の値を出しておけば、少なくとも嘘にはならない。
 */
export function matchedByLabel(matchedBy: string): string {
	switch (matchedBy) {
		case 'name': return i18n.ts._emojiApplication.matchedByName;
		case 'remoteSource': return i18n.ts._emojiApplication.matchedByRemoteSource;
		case 'fileHash': return i18n.ts._emojiApplication.matchedByFileHash;
		default: return matchedBy;
	}
}

/**
 * Reports whether more related applications can be loaded (#2960).
 *
 * **総数と取得済みの差で判断する。** 「返ってきた件数が limit と同じ」で
 * 判定すると、総数がちょうど割り切れたときに**空の追加読み込み**が出る。
 */
export function canLoadMoreRelated(counts: RelatedCounts | null, loaded: number): boolean {
	if (counts == null) return false;
	return loaded < counts.total;
}

/**
 * Resolves the thumbnail for one past application (#2960).
 *
 * **リモート絵文字は media proxy を通す。** 生 URL は `img-src 'self'` を
 * enforce している構成で黙ってブロックされる (#2957 と同じ)。
 */
export function relatedPreviewUrl(item: RelatedItem, broken: ReadonlySet<string>): string | null {
	// null = 確認できなかった / 空文字 = 申請者が drive から消した。どちらも出せない
	// (どちらなのかは `relatedImageMissingLabel` が文面で分ける)。
	if (item.url == null || item.url === '') return null;
	if (broken.has(item.id)) return null;
	if (!item.remoteHost) return item.url;
	return getProxiedImageUrl(item.url, 'emoji', false, true);
}

/**
 * Renders the placeholder text shown when the thumbnail cannot be displayed (#2960).
 *
 * **「確認できなかった」と「消された」を同じ文面に丸めない (レビュー R2-L4)。**
 * 空文字はサーバーが「drive にもう無い」と確定させた状態だが、null は
 * 確認そのものができなかった状態で、読み込み失敗も同じ。確定していないものを
 * 「画像がありません」と言い切ると、実際には残っている申請を却下しうる。
 * 審査一覧 (`custom-emojis-manager.applications.vue`) が remoteGone /
 * nameConflict に対して採っているのと同じ判断。
 */
export function relatedImageMissingLabel(item: RelatedItem, broken: ReadonlySet<string>): string {
	if (relatedImageMissingReason(item, broken) === 'gone') return i18n.ts._emojiApplication.imageGone;
	return i18n.ts._emojiApplication.imageUnknown;
}

/**
 * Reports why the thumbnail cannot be displayed (#2961).
 *
 * **判定と文面を分ける。** 審査画面の `imageUnknown` は「承認する前にもう一度
 * 読み込んでください」まで言うが、履歴を見るだけの画面には承認操作が無く、
 * 文面がそのままでは成り立たない。条件は 1 つに保ったまま、文面だけ画面ごとに
 * 決められるようにする。
 */
export function relatedImageMissingReason(item: RelatedItem, broken: ReadonlySet<string>): 'gone' | 'unknown' {
	if (item.url === '' && !broken.has(item.id)) return 'gone';
	return 'unknown';
}

/**
 * Renders the folder label shown before it is opened (#2960).
 *
 * **内訳は total を説明できる形にする。** 却下と承認しか出さないと、審査待ちや
 * 取り下げだけの履歴が「3件（却下0 / 承認0）」になり、**見なくていい履歴だと
 * 誤読される**。開く前に判断させるための表示なので、内訳が総数と合わないのは
 * それ自体が害。
 */
export function relatedSummaryLabel(counts: RelatedCounts): string {
	const parts: string[] = [];
	if (counts.rejected > 0) parts.push(i18n.tsx._emojiApplication.relatedRejected({ n: counts.rejected }));
	if (counts.approved > 0) parts.push(i18n.tsx._emojiApplication.relatedApproved({ n: counts.approved }));
	if (counts.pending > 0) parts.push(i18n.tsx._emojiApplication.relatedPending({ n: counts.pending }));
	if (counts.canceled > 0) parts.push(i18n.tsx._emojiApplication.relatedCanceled({ n: counts.canceled }));
	// **内訳が無いときは総数だけ。** 「(却下0 / 承認0)」を出すより読みやすい。
	if (parts.length === 0) return i18n.tsx._emojiApplication.relatedSummaryPlain({ total: counts.total });
	return i18n.tsx._emojiApplication.relatedSummary({
		total: counts.total,
		breakdown: parts.join(' / '),
	});
}

/**
 * Renders one status (#2960).
 *
 * **既定を「取り下げ」にしない。** 未知の status をどれかに丸めると、却下を
 * 承認と表示するような取り違えが起きる。
 */
export function relatedStatusLabel(status: string): string {
	switch (status) {
		case 'pending': return i18n.ts._emojiApplication.statusPending;
		case 'approved': return i18n.ts._emojiApplication.statusApproved;
		case 'rejected': return i18n.ts._emojiApplication.statusRejected;
		case 'canceled': return i18n.ts._emojiApplication.statusCanceled;
		default: return status;
	}
}

/**
 * Picks the cursor for the next page (#2960).
 *
 * **末尾を採る。** 先頭を渡すと同じページを永久に読み直す (id の降順なので、
 * 次に欲しいのは「いちばん小さい id より前」)。失敗して 1 件も無いときは
 * undefined を返して最初から取り直す。
 */
export function relatedNextCursor(items: readonly { id: string }[]): string | undefined {
	if (items.length === 0) return undefined;
	return items[items.length - 1].id;
}
