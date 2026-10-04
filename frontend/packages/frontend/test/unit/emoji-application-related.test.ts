/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, vi } from 'vitest';

vi.mock('@/i18n.js', () => ({
	i18n: {
		ts: {
			_emojiApplication: {
				matchedByName: 'NAME',
				matchedByRemoteSource: 'SOURCE',
				matchedByFileHash: 'IMAGE',
				statusPending: 'PENDING',
				statusApproved: 'APPROVED',
				statusRejected: 'REJECTED',
				statusCanceled: 'CANCELED',
				imageGone: 'GONE',
				imageUnknown: 'UNKNOWN',
			},
		},
		tsx: {
			_emojiApplication: {
				relatedSummary: (p: Record<string, unknown>) => `total=${p.total} (${p.breakdown})`,
				relatedSummaryPlain: (p: Record<string, unknown>) => `total=${p.total}`,
				relatedRejected: (p: Record<string, unknown>) => `rej${p.n}`,
				relatedApproved: (p: Record<string, unknown>) => `app${p.n}`,
				relatedPending: (p: Record<string, unknown>) => `pen${p.n}`,
				relatedCanceled: (p: Record<string, unknown>) => `can${p.n}`,
			},
		},
	},
}));
vi.mock('@/utility/media-proxy.js', () => ({
	getProxiedImageUrl: (url: string, ...rest: unknown[]) => `proxy(${url},${rest.join(',')})`,
}));

import { canLoadMoreRelated, matchedByLabel, relatedImageMissingLabel, relatedNextCursor, relatedPreviewUrl, relatedStatusLabel, relatedSummaryLabel } from '@/utility/emoji-application-related.js';

const counts = (total: number) => ({ total, pending: 0, approved: 0, rejected: 0, canceled: 0 });

/**
 * mk-go: 審査画面の関連する過去申請 (#2960)。
 */
describe('matchedByLabel', () => {
	test('既知の条件はラベルにする', () => {
		expect(matchedByLabel('name')).toBe('NAME');
		expect(matchedByLabel('remoteSource')).toBe('SOURCE');
		expect(matchedByLabel('fileHash')).toBe('IMAGE');
	});

	// **未知の条件でラベルを捏造しない。** サーバー側に条件が増えたとき、
	// 既存のラベルを当てると「名前が同じ」と出ているのに実際は別の理由、
	// という誤った案内になる。
	test('未知の条件は生のまま出す', () => {
		expect(matchedByLabel('aliases')).toBe('aliases');
		expect(matchedByLabel('')).toBe('');
	});
});

describe('canLoadMoreRelated', () => {
	// **総数と取得済みの差で判断する。** 「返ってきた件数が limit と同じ」で
	// 判定すると、総数がちょうど割り切れたときに空の追加読み込みが出る。
	test('残りがあるときだけ true', () => {
		expect(canLoadMoreRelated(counts(10), 5)).toBe(true);
		expect(canLoadMoreRelated(counts(5), 5)).toBe(false);
		expect(canLoadMoreRelated(counts(0), 0)).toBe(false);
	});

	test('取得前は false', () => {
		expect(canLoadMoreRelated(null, 0)).toBe(false);
	});

	// 総数より多く取れている状態 (履歴が減った) でも追加読み込みを出さない。
	test('取得済みが総数を超えていても false', () => {
		expect(canLoadMoreRelated(counts(3), 5)).toBe(false);
	});
});

describe('relatedPreviewUrl', () => {
	const none = new Set<string>();

	test('自作画像はそのまま出す', () => {
		expect(relatedPreviewUrl({ id: 'a', url: 'https://self/x.png' }, none)).toBe('https://self/x.png');
	});

	// **リモートは media proxy を通す。** 生 URL は `img-src 'self'` を
	// enforce している構成で黙ってブロックされる (#2957 と同じ)。
	test('リモート絵文字は proxy を通す', () => {
		const got = relatedPreviewUrl({ id: 'a', url: 'https://remote/x.png', remoteHost: 'remote' }, none);
		expect(got).toContain('proxy(https://remote/x.png');
		expect(got).not.toBe('https://remote/x.png');
	});

	// null = 確認できなかった / 空文字 = 申請者が drive から消した。
	test.each([
		['null', null],
		['空文字', ''],
	])('%s は出さない', (_label, url) => {
		expect(relatedPreviewUrl({ id: 'a', url }, none)).toBeNull();
	});

	test('読み込みに失敗したものは出さない', () => {
		expect(relatedPreviewUrl({ id: 'a', url: 'https://self/x.png' }, new Set(['a']))).toBeNull();
	});
});

describe('relatedImageMissingLabel', () => {
	const none = new Set<string>();

	// 空文字はサーバーが「drive にもう無い」と確定させた状態。
	test('消されたものは「ありません」', () => {
		expect(relatedImageMissingLabel({ id: 'a', url: '' }, none)).toBe('GONE');
	});

	// **確定していないものを「ありません」と言い切らない。** 実際には残って
	// いる申請を却下しうる (審査一覧が remoteGone に対して採っている判断)。
	test('確認できなかったものは「確認できません」', () => {
		expect(relatedImageMissingLabel({ id: 'a', url: null }, none)).toBe('UNKNOWN');
	});

	test('読み込みに失敗したものも「確認できません」', () => {
		expect(relatedImageMissingLabel({ id: 'a', url: 'https://self/x.png' }, new Set(['a']))).toBe('UNKNOWN');
		expect(relatedImageMissingLabel({ id: 'a', url: '' }, new Set(['a']))).toBe('UNKNOWN');
	});
});

describe('relatedSummaryLabel', () => {
	// **内訳は総数を説明できる形にする。** 却下と承認しか出さないと、審査待ちや
	// 取り下げだけの履歴が「3件（却下0 / 承認0）」になり、見なくていい履歴だと
	// 誤読される。開く前に判断させるための表示なので、それ自体が害。
	test('内訳の合計が総数と合う', () => {
		expect(relatedSummaryLabel({ total: 5, pending: 2, approved: 1, rejected: 2, canceled: 0 }))
			.toBe('total=5 (rej2 / app1 / pen2)');
	});

	test('取り下げだけでも内訳を出す', () => {
		expect(relatedSummaryLabel({ total: 3, pending: 0, approved: 0, rejected: 0, canceled: 3 }))
			.toBe('total=3 (can3)');
	});

	// 0 件の内訳は並べない (「却下0 / 承認0」より読みやすい)。
	test('0 の内訳は出さない', () => {
		expect(relatedSummaryLabel({ total: 2, pending: 0, approved: 0, rejected: 2, canceled: 0 }))
			.toBe('total=2 (rej2)');
	});

	// 未知の status だけのとき。内訳は組めないが、総数は伝える。
	test('内訳が組めなければ総数だけ出す', () => {
		expect(relatedSummaryLabel({ total: 4, pending: 0, approved: 0, rejected: 0, canceled: 0 }))
			.toBe('total=4');
	});
});

describe('relatedStatusLabel', () => {
	test.each([
		['pending', 'PENDING'],
		['approved', 'APPROVED'],
		['rejected', 'REJECTED'],
		['canceled', 'CANCELED'],
	])('%s を %s にする', (status, want) => {
		expect(relatedStatusLabel(status)).toBe(want);
	});

	// **既定を「取り下げ」にしない。** 未知の status をどれかに丸めると、
	// 却下を承認と表示するような取り違えが起きる。
	test('未知の status は生のまま出す', () => {
		expect(relatedStatusLabel('escalated')).toBe('escalated');
	});
});

describe('relatedNextCursor', () => {
	// **末尾を採る。** 先頭を渡すと同じページを永久に読み直す (id の降順)。
	test('末尾の id を返す', () => {
		expect(relatedNextCursor([{ id: 'c' }, { id: 'b' }, { id: 'a' }])).toBe('a');
	});

	test('空なら undefined (最初から取り直す)', () => {
		expect(relatedNextCursor([])).toBeUndefined();
	});
});
