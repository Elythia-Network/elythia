/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect } from 'vitest';
import { ipSearchNotice, ipSearchOutcome, ipSearchErrorKind } from '@/utility/ip-search-result.js';
import type { IPSearchSnapshot, IPSearchTotals } from '@/utility/ip-search-result.js';

function snapshot(over: Partial<IPSearchSnapshot> = {}): IPSearchSnapshot {
	return { loggingEnabled: true, hasAnyHistory: true, sinceDays: 90, retentionDays: 90, hasMore: false, ...over };
}

function totals(over: Partial<IPSearchTotals> = {}): IPSearchTotals {
	return { accountCount: 0, droppedCount: 0, hasAnyHistory: true, ...over };
}

describe('ipSearchNotice', () => {
	test('記録が有効で記録もあるなら何も言わない', () => {
		expect(ipSearchNotice(snapshot(), totals())).toBe(null);
	});

	test('記録が無効でも、残っている記録があるなら検索結果の断りだけ出す', () => {
		expect(ipSearchNotice(snapshot({ loggingEnabled: false }), totals())).toBe('loggingDisabled');
	});

	test('記録が無効で記録も無いなら、有効にする案内を出す', () => {
		expect(ipSearchNotice(snapshot({ loggingEnabled: false }), totals({ hasAnyHistory: false })))
			.toBe('loggingDisabledNoHistory');
	});

	test('記録は有効だが 1 件も無いなら、保持期間を添えて言う', () => {
		expect(ipSearchNotice(snapshot(), totals({ hasAnyHistory: false }))).toBe('noHistory');
	});

	// **累積で見る。** ページを送っている間に掃除が走って最後のページが
	// 「記録は無い」と答えても、既に引けている一覧の上で記録を否定しない。
	test('最新ページが「記録なし」でも、累積で記録があれば否定しない', () => {
		expect(ipSearchNotice(snapshot({ hasAnyHistory: false }), totals({ accountCount: 3 }))).toBe(null);
	});
});

describe('ipSearchOutcome', () => {
	test('候補が 1 件でもあれば一覧を出す', () => {
		expect(ipSearchOutcome(snapshot(), totals({ accountCount: 1 }))).toBe('accounts');
	});

	test('記録が 1 件も無いときは案内側に任せる', () => {
		expect(ipSearchOutcome(snapshot(), totals({ hasAnyHistory: false }))).toBe('accounts');
	});

	// **これが 1 周目に落とした側。** hasMore で分けると、行数が limit 以下の
	// 最後のページで「記録されていません」と、記録が残っているのに断定する。
	test('落としたものがあり続きも無いなら、「消えている」と言う', () => {
		expect(ipSearchOutcome(snapshot({ hasMore: false }), totals({ droppedCount: 2 })))
			.toBe('noneResolvable');
	});

	test('落としたものがあり続きもあるなら、「このページには」と言う', () => {
		expect(ipSearchOutcome(snapshot({ hasMore: true }), totals({ droppedCount: 2 })))
			.toBe('noneOnThisPage');
	});

	// **これが 2 周目に落とした側。** ページごとの droppedCount を見ると、
	// 落ちの無いページを引いた時点で「落としたものは無い」に化ける。
	test('落としたのが前のページでも「消えている」と言う', () => {
		expect(ipSearchOutcome(snapshot({ hasMore: false }), totals({ droppedCount: 3, accountCount: 0 })))
			.toBe('noneResolvable');
	});

	// **これが #3105 のレビュー 1 周目に落とした側。** 対象の IP 記録が窓の中に
	// 1 件も無ければ候補側のクエリは 1 回も飛ばないので、「一致が無い」とは
	// 言えない。`hasAnyHistory` はテーブル全体を見る値なので、そちらだけを見ると
	// **照合していないのに「記録されていません」と断定する。**
	test('起点が 0 件なら「比較していない」と言う', () => {
		expect(ipSearchOutcome(snapshot({ targetIPCount: 0 }), totals())).toBe('noTargetRecords');
	});

	test('起点があれば通常どおり判定する', () => {
		expect(ipSearchOutcome(snapshot({ targetIPCount: 3 }), totals())).toBe('noMatch');
	});

	// IP を直接指定する検索 (#3104) には起点という概念が無い。
	test('targetIPCount が無い検索では影響しない', () => {
		expect(ipSearchOutcome(snapshot(), totals())).toBe('noMatch');
	});

	// **打ち切った検索から「一致なし」を出さない。** 見たのは全体の一部。
	test('打ち切っていたら断定しない', () => {
		expect(ipSearchOutcome(snapshot({ truncated: true }), totals())).toBe('partial');
		expect(ipSearchOutcome(snapshot({ truncated: true, sinceDays: 7 }), totals())).toBe('partial');
	});

	// 起点が無いほうが先。打ち切りより前に「そもそも比較していない」。
	test('起点が 0 件なら打ち切りより先に伝える', () => {
		expect(ipSearchOutcome(snapshot({ targetIPCount: 0, truncated: true }), totals()))
			.toBe('noTargetRecords');
	});

	// **打ち切っていたら「すべて削除済み」と断定しない。** 言えるのは「集めた
	// 範囲の候補は全員消えている」までで、集めていない候補が居るかは分からない。
	test('打ち切っていたら「すべて」と言わない', () => {
		expect(ipSearchOutcome(snapshot({ truncated: true, hasMore: false }), totals({ droppedCount: 2 })))
			.toBe('noneResolvablePartial');
	});

	// **続きがあるならまずそれを案内する。** 「このページには」と範囲を限った
	// 言い方になるので、打ち切っていても嘘にならない。打ち切り判定を先に置くと、
	// 次ページへの案内が消えたまま「集めた範囲はすべて削除済み」と断定する
	// (ボタンは出たままなので画面内で矛盾する)。
	test('続きがあれば打ち切っていてもページの話に閉じる', () => {
		expect(ipSearchOutcome(snapshot({ truncated: true, hasMore: true }), totals({ droppedCount: 2 })))
			.toBe('noneOnThisPage');
	});

	// 打ち切っていなければ従来どおり言い切れる。
	test('打ち切っていなければ「すべて削除済み」と言える', () => {
		expect(ipSearchOutcome(snapshot({ hasMore: false }), totals({ droppedCount: 2 })))
			.toBe('noneResolvable');
		expect(ipSearchOutcome(snapshot({ hasMore: true }), totals({ droppedCount: 2 })))
			.toBe('noneOnThisPage');
	});

	test('落としたものが無く窓が保持期間以上なら、一致なしと言い切る', () => {
		expect(ipSearchOutcome(snapshot({ sinceDays: 90, retentionDays: 90 }), totals())).toBe('noMatch');
	});

	test('窓が保持期間より狭いなら、期間を広げる余地を残す', () => {
		expect(ipSearchOutcome(snapshot({ sinceDays: 7, retentionDays: 90 }), totals())).toBe('noMatchInPeriod');
	});

	test('窓が保持期間より広ければ言い切ってよい', () => {
		expect(ipSearchOutcome(snapshot({ sinceDays: 365, retentionDays: 90 }), totals())).toBe('noMatch');
	});
});

// **全入力を網羅して不変条件を固定する。**
//
// この判定は 3 周続けて間違えており、毎周「画面が事実と違うことを言う」を 1 つ直して
// 隣の枝で 1 つ作っている。原因は個別の判断ではなく、**組み合わせを網羅していな
// かったこと** — 以前の表は 16 通りしか回さず、候補の有無・起点・打ち切りの 3 軸を
// 一度も振っていなかった。ここで全部回し、言ってよいことを枝ごとに固定する。
describe('ipSearchNotice / ipSearchOutcome の全組み合わせ', () => {
	type Case = { s: IPSearchSnapshot; t: IPSearchTotals; label: string };

	function allCases(): Case[] {
		const out: Case[] = [];
		for (const loggingEnabled of [true, false]) {
			for (const hasAnyHistory of [true, false]) {
				for (const accountCount of [0, 2]) {
					for (const droppedCount of [0, 2]) {
						for (const hasMore of [true, false]) {
							for (const targetIPCount of [undefined, 0, 3] as const) {
								for (const truncated of [undefined, false, true] as const) {
									for (const sinceDays of [7, 90]) {
										out.push({
											s: { loggingEnabled, hasAnyHistory, sinceDays, retentionDays: 90, hasMore, targetIPCount, truncated },
											t: { accountCount, droppedCount, hasAnyHistory },
											label: `logging=${loggingEnabled} history=${hasAnyHistory} accounts=${accountCount} dropped=${droppedCount} hasMore=${hasMore} targetIPs=${targetIPCount} truncated=${truncated} since=${sinceDays}`,
										});
									}
								}
							}
						}
					}
				}
			}
		}
		return out;
	}

	test('候補が無いときは必ず何かを伝える', () => {
		for (const c of allCases()) {
			if (c.t.accountCount > 0) continue;
			const said = ipSearchNotice(c.s, c.t) != null || ipSearchOutcome(c.s, c.t) !== 'accounts';
			expect(said, c.label).toBe(true);
		}
	});

	test('候補があれば一覧を出す', () => {
		for (const c of allCases()) {
			if (c.t.accountCount === 0) continue;
			expect(ipSearchOutcome(c.s, c.t), c.label).toBe('accounts');
		}
	});

	// **「一致が無い」と言えるのは全部見たときだけ。** 起点が無い / 打ち切った /
	// 候補を落とした、のいずれかがあれば断定できない。
	test('調べきっていないときに「一致なし」と断定しない', () => {
		for (const c of allCases()) {
			const outcome = ipSearchOutcome(c.s, c.t);
			if (outcome !== 'noMatch' && outcome !== 'noMatchInPeriod') continue;
			expect(c.s.truncated, c.label).not.toBe(true);
			expect(c.s.targetIPCount, c.label).not.toBe(0);
			expect(c.t.droppedCount, c.label).toBe(0);
		}
	});

	// **「すべて削除済み」と言い切れるのは、切っておらず続きも無いときだけ。**
	test('打ち切った検索や続きがあるときに「すべて削除済み」と断定しない', () => {
		for (const c of allCases()) {
			if (ipSearchOutcome(c.s, c.t) !== 'noneResolvable') continue;
			expect(c.s.truncated, c.label).not.toBe(true);
			expect(c.s.hasMore, c.label).toBe(false);
			expect(c.t.droppedCount, c.label).toBeGreaterThan(0);
		}
	});

	// **「このページには」と言えるのは続きがあるときだけ。**
	test('続きが無いのに「このページには」と言わない', () => {
		for (const c of allCases()) {
			if (ipSearchOutcome(c.s, c.t) !== 'noneOnThisPage') continue;
			expect(c.s.hasMore, c.label).toBe(true);
			expect(c.t.droppedCount, c.label).toBeGreaterThan(0);
		}
	});

	// **打ち切りを理由にする 2 つは、実際に打ち切ったときだけ。**
	test('打ち切っていないのに打ち切りを理由にしない', () => {
		for (const c of allCases()) {
			const outcome = ipSearchOutcome(c.s, c.t);
			if (outcome === 'partial') {
				expect(c.s.truncated, c.label).toBe(true);
				expect(c.t.droppedCount, c.label).toBe(0);
			}
			if (outcome === 'noneResolvablePartial') {
				expect(c.s.truncated, c.label).toBe(true);
				expect(c.s.hasMore, c.label).toBe(false);
				expect(c.t.droppedCount, c.label).toBeGreaterThan(0);
			}
		}
	});

	// **「比較していない」は起点が 0 件のときに限る。**
	test('起点がある検索で「比較していない」と言わない', () => {
		for (const c of allCases()) {
			if (ipSearchOutcome(c.s, c.t) !== 'noTargetRecords') continue;
			expect(c.s.targetIPCount, c.label).toBe(0);
			expect(c.t.hasAnyHistory, c.label).toBe(true);
			expect(c.t.accountCount, c.label).toBe(0);
		}
	});

	// **記録が無いと分かっているときは必ず案内側が言う。**
	test('記録が無いときは案内が出る', () => {
		for (const c of allCases()) {
			if (c.t.hasAnyHistory) continue;
			expect(ipSearchNotice(c.s, c.t), c.label).not.toBe(null);
		}
	});

	// 案内は記録の状態だけで決まり、候補や打ち切りには依らない。
	test('案内は記録の状態だけで決まる', () => {
		for (const c of allCases()) {
			const notice = ipSearchNotice(c.s, c.t);
			if (notice === 'noHistory') {
				expect(c.s.loggingEnabled, c.label).toBe(true);
				expect(c.t.hasAnyHistory, c.label).toBe(false);
			}
			if (notice === 'loggingDisabled' || notice === 'loggingDisabledNoHistory') {
				expect(c.s.loggingEnabled, c.label).toBe(false);
			}
		}
	});
});

describe('ipSearchErrorKind', () => {
	test.each([
		['ROLE_PERMISSION_DENIED', true, 'notPermitted'],
		['PERMISSION_DENIED', false, 'notPermitted'],
		['CREDENTIAL_REQUIRED', true, 'sessionExpired'],
		['AUTHENTICATION_FAILED', false, 'sessionExpired'],
		['INVALID_PARAM', true, 'notAnIp'],
		['INVALID_PARAM', false, 'pagingLimit'],
		['RATE_LIMIT_EXCEEDED', true, 'rateLimited'],
		['RATE_LIMIT_EXCEEDED', false, 'rateLimited'],
		['INTERNAL_ERROR', true, 'failed'],
	] as const)('%s (first=%s) -> %s', (code, first, want) => {
		expect(ipSearchErrorKind({ code }, first)).toBe(want);
	});

	// **サーバーが返したエラーでないものを「サーバーのログを確認してください」に
	// しない。** 通信断ではログに何も残らない。
	test.each([
		[new Error('network down')],
		[{ message: 'no code' }],
		[{ code: 42 }],
		[null],
		[undefined],
	])('サーバー由来でないエラーは通信失敗として扱う (%s)', (err) => {
		expect(ipSearchErrorKind(err, true)).toBe('networkFailed');
	});
});
