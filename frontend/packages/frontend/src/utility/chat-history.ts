/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Elythia: チャットのホームの一覧で、新しい10会話より古い会話も読めるようにする (#3653)。
// 本家の chat/history は limit (上限100) しか持たず、続きを読む手段が無い。/api/i の
// hasUnreadChatMessages は全部の会話を数えるので、11番目より古い会話に未読が残ると、
// 画面のどこにも出ないままバッジだけが付き続けていた。Elythia の chat/history は
// untilId (前のページの最後のメッセージの id) を受けるので、それで続きを読む。

import { computed, ref } from 'vue';

/** The number of conversations the list adds at a time. */
export const CHAT_HISTORY_PAGE = 10;

/** Upstream's maximum of `limit` for chat/history. */
export const CHAT_HISTORY_MAX_LIMIT = 100;

/** Fetches one page of chat/history. */
export type ChatHistoryFetcher<T extends { id: string }> = (params: { limit: number; untilId?: string }) => Promise<T[]>;

/**
 * Reads the latest `count` conversations, in pages of up to
 * CHAT_HISTORY_MAX_LIMIT.
 *
 * `more` is true when there is at least one older conversation.
 */
export async function fetchChatHistory<T extends { id: string }>(fetchPage: ChatHistoryFetcher<T>, count: number): Promise<{ items: T[]; more: boolean }> {
	const items: T[] = [];
	// 一覧の再読み込みは、読み込んだ分をまとめて取り直す。最初のページだけを取り直すと、
	// 古い会話を開いて既読にして戻っても、その行の未読の印が消えない。
	// 続きがあるかは 1 件余分に読んで確かめる。満杯のページだけで決めると、会話が
	// ちょうど count 件のときに、押しても何も増えない「もっと見る」が出る。
	while (items.length <= count) {
		const limit = Math.min(count + 1 - items.length, CHAT_HISTORY_MAX_LIMIT);
		const untilId = items.length > 0 ? items[items.length - 1].id : undefined;
		const page = await fetchPage(untilId == null ? { limit } : { limit, untilId });
		// untilId を知らない backend (本家) は先頭から返し直すので、既に持っている行が
		// 混ざったら続きは無いものとする。
		const fresh = page.filter(m => !items.some(i => i.id === m.id));
		items.push(...fresh);
		if (items.length > count) break;
		if (page.length < limit || fresh.length < page.length) return { items, more: false };
	}
	return { items: items.slice(0, count), more: true };
}

/** Fetches one page of chat/history for DMs (`room: false`) or rooms. */
export type ChatHistoryKindFetcher<T extends { id: string }> = (room: boolean, params: { limit: number; untilId?: string }) => Promise<T[]>;

/**
 * Keeps how many conversations of each kind the list shows, and whether
 * there are more.
 */
export function createChatHistoryLoader<T extends { id: string }>(fetchPage: ChatHistoryKindFetcher<T>) {
	const counts = { user: CHAT_HISTORY_PAGE, room: CHAT_HISTORY_PAGE };
	const userHasMore = ref(false);
	const roomHasMore = ref(false);
	const fetching = ref(false);
	const hasMore = computed(() => userHasMore.value || roomHasMore.value);

	/** Reads both kinds again. Returns null when a read is already running. */
	async function refresh(): Promise<{ user: T[]; room: T[] } | null> {
		if (fetching.value) return null;
		fetching.value = true;
		try {
			const [user, room] = await Promise.all([
				fetchChatHistory(params => fetchPage(false, params), counts.user),
				fetchChatHistory(params => fetchPage(true, params), counts.room),
			]);
			userHasMore.value = user.more;
			roomHasMore.value = room.more;
			return { user: user.items, room: room.items };
		} finally {
			// 失敗しても戻す。戻さないと、以後の再読み込みがすべて素通りになる。
			fetching.value = false;
		}
	}

	/** Shows CHAT_HISTORY_PAGE more conversations of each kind that has more. */
	function loadMore(): Promise<{ user: T[]; room: T[] } | null> {
		if (userHasMore.value) counts.user += CHAT_HISTORY_PAGE;
		if (roomHasMore.value) counts.room += CHAT_HISTORY_PAGE;
		return refresh();
	}

	return { fetching, hasMore, refresh, loadMore };
}
