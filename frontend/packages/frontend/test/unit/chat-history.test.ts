/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import { CHAT_HISTORY_MAX_LIMIT, createChatHistoryLoader, fetchChatHistory } from '@/utility/chat-history.js';

// conversations 個の会話を、最新の id の降順に持つサーバーの代わり。
function server(conversations: number) {
	const all = Array.from({ length: conversations }, (_, i) => ({ id: String(conversations - i).padStart(4, '0') }));
	const calls: { limit: number; untilId?: string }[] = [];
	const fetchPage = async (params: { limit: number; untilId?: string }) => {
		calls.push(params);
		// 止まらない実装でテストごと固まらないよう、読みすぎたら落とす。
		if (calls.length > 20) throw new Error('too many pages');
		const rest = params.untilId == null ? all : all.filter(m => m.id < params.untilId!);
		return rest.slice(0, params.limit);
	};
	return { all, calls, fetchPage };
}

describe('チャットのホームの一覧の続き (#3653)', () => {
	test('最初は10会話を1回で読み、続きがあることを1件余分に読んで確かめる', async () => {
		const s = server(11);
		const res = await fetchChatHistory(s.fetchPage, 10);
		expect(res.items.map(m => m.id)).toEqual(s.all.slice(0, 10).map(m => m.id));
		expect(res.more).toBe(true);
		expect(s.calls).toEqual([{ limit: 11 }]);
	});

	test('会話がちょうど10件なら、続きは無い', async () => {
		const s = server(10);
		const res = await fetchChatHistory(s.fetchPage, 10);
		expect(res.items).toHaveLength(10);
		expect(res.more).toBe(false);
		expect(s.calls).toEqual([{ limit: 11 }]);
	});

	test('11番目より古い会話も、数を増やせば読める', async () => {
		const s = server(11);
		const res = await fetchChatHistory(s.fetchPage, 20);
		expect(res.items.map(m => m.id)).toEqual(s.all.map(m => m.id));
		expect(res.more).toBe(false);
		// 足りないページが返ったら、それ以上は読まない。
		expect(s.calls).toEqual([{ limit: 21 }]);
	});

	test('上限の100を超える数は、前のページの最後の id から続けて読む', async () => {
		const s = server(250);
		const res = await fetchChatHistory(s.fetchPage, 230);
		expect(res.items.map(m => m.id)).toEqual(s.all.slice(0, 230).map(m => m.id));
		expect(res.more).toBe(true);
		expect(s.calls).toEqual([
			{ limit: CHAT_HISTORY_MAX_LIMIT },
			{ limit: CHAT_HISTORY_MAX_LIMIT, untilId: s.all[99].id },
			{ limit: 31, untilId: s.all[199].id },
		]);
	});

	test('untilId を無視する backend では、同じ会話を重ねずに止まる', async () => {
		const s = server(250);
		const ignoring = (params: { limit: number; untilId?: string }) => {
			// 止まらない実装でテストごと固まらないよう、読みすぎたら落とす。
			if (s.calls.length > 5) throw new Error('too many pages');
			return s.fetchPage({ limit: params.limit });
		};
		const res = await fetchChatHistory(ignoring, 230);
		expect(res.items.map(m => m.id)).toEqual(s.all.slice(0, 100).map(m => m.id));
		expect(res.more).toBe(false);
	});

	test('ちょうど上限の100件のときも、続きの有無を1件で確かめる', async () => {
		const s = server(100);
		const res = await fetchChatHistory(s.fetchPage, 100);
		expect(res.items).toHaveLength(100);
		expect(res.more).toBe(false);
		expect(s.calls).toEqual([{ limit: 100 }, { limit: 1, untilId: s.all[99].id }]);
	});

	test('会話が無ければ空で、続きは無い', async () => {
		const s = server(0);
		expect(await fetchChatHistory(s.fetchPage, 10)).toEqual({ items: [], more: false });
	});
});

describe('チャットのホームの一覧の読み込み (#3653)', () => {
	function servers(users: number, rooms: number) {
		const u = server(users);
		const r = server(rooms);
		return { u, r, fetchPage: (room: boolean, params: { limit: number; untilId?: string }) => (room ? r : u).fetchPage(params) };
	}

	test('「もっと見る」は、続きのある方だけを増やす', async () => {
		const s = servers(25, 5);
		const loader = createChatHistoryLoader(s.fetchPage);
		let res = await loader.refresh();
		expect(res?.user).toHaveLength(10);
		expect(res?.room).toHaveLength(5);
		expect(loader.hasMore.value).toBe(true);

		res = await loader.loadMore();
		expect(res?.user).toHaveLength(20);
		expect(s.r.calls.at(-1)).toEqual({ limit: 11 });
		expect(loader.hasMore.value).toBe(true);

		res = await loader.loadMore();
		expect(res?.user).toHaveLength(25);
		expect(loader.hasMore.value).toBe(false);

		// 再読み込みは、読み込んだ数までまとめて取り直す。
		res = await loader.refresh();
		expect(res?.user).toHaveLength(25);
	});

	test('読んでいる間の再読み込みは素通りし、失敗しても次は読める', async () => {
		let fail = true;
		const s = servers(3, 0);
		const loader = createChatHistoryLoader(async (room: boolean, params: { limit: number; untilId?: string }) => {
			if (fail) throw new Error('boom');
			return s.fetchPage(room, params);
		});
		const first = loader.refresh();
		expect(loader.fetching.value).toBe(true);
		expect(await loader.refresh()).toBeNull();
		await expect(first).rejects.toThrow('boom');
		expect(loader.fetching.value).toBe(false);

		fail = false;
		const res = await loader.refresh();
		expect(res?.user).toHaveLength(3);
	});
});
