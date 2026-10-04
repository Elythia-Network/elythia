/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, beforeEach, vi } from 'vitest';

const h = vi.hoisted(() => ({ api: vi.fn() }));
vi.mock('@/utility/misskey-api.js', () => ({
	misskeyApi: (...args: unknown[]) => h.api(...args),
	pendingApiRequestsCount: { value: 0 },
}));

import { computed } from 'vue';
import { Paginator } from '@/utility/paginator.js';

function item(id: string) {
	return { id };
}

// **フィクスチャは ASC (古い順) で書く。** `sinceId` だけを送るページングは
// バックエンドが昇順で返す (mk-go `internal/repository/pagination.go` / upstream
// `QueryService.makePaginationQuery`)。降順のフィクスチャで書くと、production の
// `toReversed()` を外しても通ってしまう。
async function loaded(initial: string[] = []) {
	h.api.mockReset();
	h.api.mockResolvedValueOnce(initial.map(item));
	const p = new Paginator('i/notifications', { limit: 15 });
	await p.init();
	return p;
}

// 桁を揃えないと辞書順が id の時系列順と食い違う。
const nid = (n: number) => `n${String(n).padStart(3, '0')}`;

function lastRequest() {
	return h.api.mock.calls.at(-1)?.[1] as Record<string, unknown>;
}

/**
 * mk-go: 再接続時の取りこぼし回収で、queue に同じアイテムが二重に積まれたり、
 * 並び順が崩れたりするのを防ぐ (#3132)。
 *
 * `unshiftItems` / `prepend` は **items との** 重複を弾くが `aheadQueue` は見ない。
 * `enqueue` と `fetchNewer({toQueue:true})` はどちらも見ていなかった。再接続の
 * 直後は差分取得とストリーミング受信が同時に走るので、この窓が現実的になる。
 */
describe('Paginator の queue 重複排除', () => {
	beforeEach(() => h.api.mockReset());

	test('同じアイテムを二度 enqueue しても 1 件しか積まれない', async () => {
		const p = await loaded();
		p.enqueue(item('n1') as never);
		p.enqueue(item('n1') as never);
		expect(p.queuedAheadItemsCount.value).toBe(1);
	});

	test('既に一覧にあるアイテムは enqueue しない', async () => {
		const p = await loaded(['n1']);
		p.enqueue(item('n1') as never);
		expect(p.queuedAheadItemsCount.value).toBe(0);
	});

	test('別のアイテムはそのまま積まれる', async () => {
		const p = await loaded(['n1']);
		p.enqueue(item('n2') as never);
		p.enqueue(item('n3') as never);
		expect(p.queuedAheadItemsCount.value).toBe(2);
	});

	test('fetchNewer(toQueue) は queue に既にあるものを積み直さない', async () => {
		const p = await loaded(['n1']);
		p.enqueue(item('n2') as never);
		h.api.mockResolvedValueOnce([item('n2'), item('n3')]);
		await p.fetchNewer({ toQueue: true });
		expect(p.queuedAheadItemsCount.value).toBe(2);
	});

	test('fetchNewer(toQueue) は一覧に既にあるものを積まない', async () => {
		const p = await loaded(['n1']);
		h.api.mockResolvedValueOnce([item('n1')]);
		await p.fetchNewer({ toQueue: true });
		expect(p.queuedAheadItemsCount.value).toBe(0);
	});

	test('fetchNewer(toQueue) は 1 バッチ内の重複も落とす', async () => {
		const p = await loaded(['n1']);
		h.api.mockResolvedValueOnce([item('n2'), item('n2')]);
		await p.fetchNewer({ toQueue: true });
		p.releaseQueue();
		expect(p.items.value.map(x => x.id)).toEqual(['n2', 'n1']);
	});

	test('ASC で返るバッチを newest-first で積む', async () => {
		const p = await loaded(['n0']);
		h.api.mockResolvedValueOnce([item('n1'), item('n2')]);
		await p.fetchNewer({ toQueue: true });
		p.releaseQueue();
		expect(p.items.value.map(x => x.id)).toEqual(['n2', 'n1', 'n0']);
	});

	test('間引きが起きても並び順が崩れない', async () => {
		// 取得中にストリーミングで n2 が先に届いた状況。バッチから n2 が抜けるので、
		// 素朴な unshift だと n1 が n2 より上に来る。
		const p = await loaded(['n0']);
		p.enqueue(item('n2') as never);
		h.api.mockResolvedValueOnce([item('n1'), item('n2'), item('n3')]);
		await p.fetchNewer({ toQueue: true });
		p.releaseQueue();
		expect(p.items.value.map(x => x.id)).toEqual(['n3', 'n2', 'n1', 'n0']);
	});

	test('unshiftItems で一覧に入ったものも queue から消える', async () => {
		// バッジだけ残ると、押しても何も増えずに 0 に戻る。`prepend` だけでなく
		// items に入れる経路すべてで引き取る必要がある。
		const p = await loaded(['n0']);
		p.enqueue(item('n1') as never);
		expect(p.queuedAheadItemsCount.value).toBe(1);
		h.api.mockResolvedValueOnce([item('n1')]);
		await p.fetchNewer({ toQueue: false });
		expect(p.queuedAheadItemsCount.value).toBe(0);
		expect(p.items.value.map(x => x.id)).toEqual(['n1', 'n0']);
	});

	test('pushItems は既に持っている id を追記しない', async () => {
		// `init()` が items を空にして API を待つ間に、背景の `fetchNewer` が
		// `unshiftItems` で入れていることがある (:key が重複する)。
		const p = await loaded(['n0']);
		p.pushItems([item('n0'), item('n5')] as never);
		expect(p.items.value.map(x => x.id)).toEqual(['n0', 'n5']);
	});

	test('一覧に入ってしまった後でも prepend で queue から引き取れる', async () => {
		// `pushItems` (init / fetchOlder) は引き取らないので、items にあるのに
		// queue にも残る状態は作れる。`prepend` の early return より前で引き取って
		// いないと、ここから救えない。
		const p = await loaded(['n0']);
		p.enqueue(item('n1') as never);
		p.items.value.unshift(item('n1') as never);
		expect(p.queuedAheadItemsCount.value).toBe(1);
		p.prepend(item('n1') as never);
		expect(p.queuedAheadItemsCount.value).toBe(0);
	});

	test('既に一覧にあるものが queue に残っていても unshiftItems で引き取る', async () => {
		// `pushItems` (init / fetchOlder) は引き取らないので、items にあるのに
		// queue にも残る状態は作れる。引き取り対象を「実際に挿入した分」に絞ると
		// ここが救えなくなる。
		const p = await loaded(['n0']);
		p.enqueue(item('n1') as never);
		p.items.value.unshift(item('n1') as never);
		h.api.mockResolvedValueOnce([item('n1')]);
		await p.fetchNewer({ toQueue: false });
		expect(p.queuedAheadItemsCount.value).toBe(0);
	});

	test('pushItems は 1 バッチ内の重複も落とす', async () => {
		const p = await loaded(['n0']);
		p.pushItems([item('n5'), item('n5')] as never);
		expect(p.items.value.map(x => x.id)).toEqual(['n0', 'n5']);
	});

	test('queue が溢れたら、残すのは id の新しいほう', async () => {
		// 到着順と id 順が一致しない状態を作る (受信と取得が混ざるとこうなる)。
		const p = await loaded([]);
		for (let i = 200; i >= 101; i--) p.enqueue(item(nid(i)) as never);
		expect(p.queuedAheadItemsCount.value).toBe(100);
		h.api.mockResolvedValueOnce(Array.from({ length: 30 }, (_, k) => item(nid(k + 1))));
		await p.fetchNewer({ toQueue: true });
		expect(p.queuedAheadItemsCount.value).toBe(100);
		p.releaseQueue();
		// items は 30 件に trim されるので、観測できるのは上位 30 件。
		expect(p.items.value.map(x => x.id)).toEqual(Array.from({ length: 30 }, (_, k) => nid(200 - k)));
	});

	test('enqueue で溢れたときも、捨てるのは最古', async () => {
		// `pop()` は「末尾が最古」を前提にしている。並べ直しが入った今、その前提は
		// 成り立たない (最新を捨てうる)。
		const p = await loaded([]);
		// **到着順を id 降順にする。** こうすると queue は id 昇順 (末尾が最新) に
		// なり、`pop()` はいちばん新しいものを捨てる。
		for (let i = 100; i >= 1; i--) p.enqueue(item(nid(i)) as never);
		expect(p.queuedAheadItemsCount.value).toBe(100);
		p.enqueue(item(nid(300)) as never);
		p.releaseQueue();
		const ids = p.items.value.map(x => x.id);
		expect(ids).toContain(nid(300));
		expect(ids).not.toContain(nid(1));
	});

	test('prepend は queue に残ったコピーを引き取る', async () => {
		// queue に積んだものが先に items へ入ると、バッジだけ残って押しても増えない。
		const p = await loaded(['n0']);
		p.enqueue(item('n1') as never);
		expect(p.queuedAheadItemsCount.value).toBe(1);
		p.prepend(item('n1') as never);
		expect(p.queuedAheadItemsCount.value).toBe(0);
		expect(p.items.value.map(x => x.id)).toEqual(['n1', 'n0']);
	});
});

/**
 * mk-go: `pushItems` の重複排除は offsetMode を詰まらせる (#3132)。
 *
 * あちらは `offset: items.length` で次の位置を決めるので、1 ページ丸ごと既知
 * だったときに items が伸びず、同じページを取り続ける (「もっと見る」が押しても
 * 何も起きないボタンになる)。並びが動く一覧で窓が 1 ページぶんずれると起きる。
 */
describe('Paginator の offsetMode', () => {
	beforeEach(() => h.api.mockReset());

	test('offsetMode では重複を落とさない', async () => {
		h.api.mockResolvedValueOnce([item('a'), item('b')]);
		const p = new Paginator('users/search', { limit: 2, offsetMode: true } as never);
		await p.init();
		h.api.mockResolvedValueOnce([item('a'), item('b')]);
		await p.fetchOlder();
		expect(p.items.value.map(x => x.id)).toEqual(['a', 'b', 'a', 'b']);
	});
});

/**
 * mk-go: 再接続の穴埋めは起点を呼び出し側が持つ (#3132)。既定の `getNewestId()` は
 * queue があればその最大 id を返すので、再接続後にストリーミングが 1 件でも届くと
 * 起点が切断中の穴を飛び越える。
 */
describe('Paginator.fetchNewer の起点', () => {
	beforeEach(() => h.api.mockReset());

	test('既定では手持ちの最新 id を起点にする', async () => {
		const p = await loaded(['n0', 'n1']);
		h.api.mockResolvedValueOnce([]);
		await p.fetchNewer({});
		expect(lastRequest().sinceId).toBe('n1');
	});

	test('queue にあるものが起点を押し上げる (穴を飛び越える既定の挙動)', async () => {
		const p = await loaded(['n1']);
		p.enqueue(item('n9') as never);
		h.api.mockResolvedValueOnce([]);
		await p.fetchNewer({ toQueue: true });
		expect(lastRequest().sinceId).toBe('n9');
	});

	test('sinceId を渡すとそちらが起点になる', async () => {
		const p = await loaded(['n1']);
		p.enqueue(item('n9') as never);
		h.api.mockResolvedValueOnce([]);
		await p.fetchNewer({ toQueue: true, sinceId: 'n1' });
		expect(lastRequest().sinceId).toBe('n1');
	});

	test('起点を明示した取得は、待つ間に届いた新しいものの下へ入る', async () => {
		// 再接続の穴埋めは過去の時点を起点にするので、返るバッチは取得中に
		// ストリーミングで入ったものより古い。そのまま先頭へ積むと最新が埋まる。
		const p = await loaded(['n0']);
		p.prepend(item('n9') as never);
		h.api.mockResolvedValueOnce([item('n1'), item('n2')]);
		await p.fetchNewer({ sinceId: 'n0', toQueue: false });
		expect(p.items.value.map(x => x.id)).toEqual(['n9', 'n2', 'n1', 'n0']);
	});

	test('起点を明示しない取得の並びは変えない', async () => {
		const p = await loaded(['n0']);
		h.api.mockResolvedValueOnce([item('n1'), item('n2')]);
		await p.fetchNewer({ toQueue: false });
		expect(p.items.value.map(x => x.id)).toEqual(['n2', 'n1', 'n0']);
	});

	test('起点を明示した取得は一覧全体を並べ直す', async () => {
		// 差し込んだ分だけ merge する形にすると、既存の並びが崩れたままになる。
		const p = await loaded(['n5']);
		p.items.value.push(item('n9') as never);
		h.api.mockResolvedValueOnce([item('n6')]);
		await p.fetchNewer({ sinceId: 'n0', toQueue: false });
		expect(p.items.value.map(x => x.id)).toEqual(['n9', 'n6', 'n5']);
	});

	test('init は世代を進める', async () => {
		const p = await loaded(['n0']);
		const before = p.generation;
		h.api.mockResolvedValueOnce([]);
		await p.reload();
		expect(p.generation).toBe(before + 1);
	});

	test('世代は一覧を空にした時点で進む', async () => {
		// 完了時に進めると、入れ替えの最中は「古い世代のまま」に見える。早く
		// 無効化するほうが安全側 (控えを持っている側が待たずに捨てられる)。
		const p = await loaded(['n0']);
		let genDuring = -1;
		let itemsDuring = -1;
		h.api.mockImplementationOnce(async () => {
			genDuring = p.generation;
			itemsDuring = p.items.value.length;
			return [];
		});
		await p.reload();
		expect(itemsDuring).toBe(0);
		expect(genDuring).toBe(p.generation);
	});

	test('取得ごとの追加パラメータをリクエストに載せる', async () => {
		// 通知一覧の穴埋めは `markAsRead: false` で撃つ (取得そのものが既読化を
		// 伴うため)。載らないと、背景の取得で未読が消える。
		const p = await loaded(['n0']);
		h.api.mockResolvedValueOnce([]);
		await p.fetchNewer({ sinceId: 'n0', params: { markAsRead: false } as never });
		expect(lastRequest().markAsRead).toBe(false);
	});

	test('追加パラメータは起点や件数を上書きできない', async () => {
		// 共通基盤の公開 API なので、呼び出し側がページングの前提を黙って
		// 差し替えられる形にしない (`IPaginator` 経由だと型でも止められない)。
		const p = await loaded(['n0']);
		h.api.mockResolvedValueOnce([]);
		await p.fetchNewer({ sinceId: 'n5', params: { sinceId: 'zzz', limit: 1 } as never });
		expect(lastRequest().sinceId).toBe('n5');
		expect(lastRequest().limit).not.toBe(1);
	});

	test('reload の追加パラメータもリクエストに載る', async () => {
		// 通知一覧の穴埋めは、一覧が空のとき `reload()` の枝を通る。そこで
		// `markAsRead: false` が落ちると、背景の取得が全既読にする。
		const p = await loaded(['n0']);
		h.api.mockResolvedValueOnce([]);
		await p.reload({ markAsRead: false } as never);
		expect(lastRequest().markAsRead).toBe(false);
	});

	test('追加パラメータは静的 params も上書きできない', async () => {
		// `MkStreamingNotesTimeline` は `src: 'directs'` で `{visibility:'specified'}`
		// を静的 params に置いている。これは「この一覧が何であるか」そのもの。
		h.api.mockReset();
		h.api.mockResolvedValueOnce([]);
		const p = new Paginator('i/notifications', {
			limit: 15,
			params: { excludeTypes: ['reaction'] as never },
		});
		await p.init();
		h.api.mockResolvedValueOnce([]);
		await p.fetchNewer({ sinceId: 'n5', params: { excludeTypes: ['follow'] } as never });
		expect(lastRequest().excludeTypes).toEqual(['reaction']);
	});

	test('追加パラメータは一覧のフィルタも上書きできない', async () => {
		// `computedParams` は「この一覧が何であるか」を決めている。ページングより
		// 影響が大きいので、こちらも守る。
		h.api.mockReset();
		h.api.mockResolvedValueOnce([]);
		const p = new Paginator('i/notifications', {
			limit: 15,
			computedParams: computed(() => ({ excludeTypes: ['reaction'] as never })),
		});
		await p.init();
		h.api.mockResolvedValueOnce([]);
		await p.fetchNewer({ sinceId: 'n5', params: { excludeTypes: ['follow'] } as never });
		expect(lastRequest().excludeTypes).toEqual(['reaction']);
	});

	test('起点を明示しない取得は既存の並びに手を入れない', async () => {
		// upstream は items の並びを保証していない (`getNewestId` が毎回ソート
		// しているのがその現れ)。穴埋め以外の経路で並べ直すと、既存の見え方を
		// 黙って変えることになる。
		const p = await loaded(['n5']);
		p.items.value.push(item('n9') as never);
		h.api.mockResolvedValueOnce([item('n6')]);
		await p.fetchNewer({ toQueue: false });
		expect(p.items.value.map(x => x.id)).toEqual(['n6', 'n5', 'n9']);
	});
});
