/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { shallowRef, computed, markRaw, watch } from 'vue';
import * as Misskey from 'misskey-js';
import { misskeyApi, misskeyApiGet } from '@/utility/misskey-api.js';
import { get, set } from '@/utility/idb-proxy.js';

const storageCache = await get('emojis');
export const customEmojis = shallowRef<Misskey.entities.EmojiSimple[]>(Array.isArray(storageCache) ? storageCache : []);
export const customEmojiCategories = computed<[ ...string[], null ]>(() => {
	const categories = new Set<string>();
	for (const emoji of customEmojis.value) {
		if (emoji.category && emoji.category !== 'null') {
			categories.add(emoji.category);
		}
	}
	return markRaw([...Array.from(categories), null]);
});

export const customEmojisMap = new Map<string, Misskey.entities.EmojiSimple>();
watch(customEmojis, emojis => {
	customEmojisMap.clear();
	for (const emoji of emojis) {
		customEmojisMap.set(emoji.name, emoji);
	}
}, { immediate: true });

export function addCustomEmoji(emoji: Misskey.entities.EmojiSimple) {
	customEmojis.value = [emoji, ...customEmojis.value];
	set('emojis', customEmojis.value);
}

export function updateCustomEmojis(emojis: Misskey.entities.EmojiSimple[]) {
	const emojisByName = new Map<string, Misskey.entities.EmojiSimple>();
	for (const emoji of emojis) {
		if (!emojisByName.has(emoji.name)) {
			emojisByName.set(emoji.name, emoji);
		}
	}
	customEmojis.value = customEmojis.value.map(item => emojisByName.get(item.name) ?? item);
	set('emojis', customEmojis.value);
}

export function removeCustomEmojis(emojis: Misskey.entities.EmojiSimple[]) {
	const emojiNames = new Set(emojis.map(emoji => emoji.name));
	customEmojis.value = customEmojis.value.filter(item => !emojiNames.has(item.name));
	set('emojis', customEmojis.value);
}

export async function fetchCustomEmojis(force = false) {
	const now = Date.now();

	let res;
	if (force) {
		res = await misskeyApi('emojis', {});
	} else {
		const lastFetchedAt = await get('lastEmojisFetchedAt');
		if (lastFetchedAt && (now - lastFetchedAt) < 1000 * 60 * 60) return;
		res = await misskeyApiGet('emojis', {});
	}

	customEmojis.value = res.emojis;
	set('emojis', res.emojis);
	set('lastEmojisFetchedAt', now);
}

/**
 * Refreshes the custom emoji list while the client boots.
 * Waits for the request only when there is no cached list.
 */
export async function refreshCustomEmojisOnBoot(): Promise<void> {
	// Elythia: 手元 (IndexedDB) に一覧があるときは、/api/emojis の応答を待たずに画面を出す (#3613)。
	// 取得は裏で続け、返ってきたら customEmojis を差し替える。描いてある絵文字 (MkCustomEmoji) と
	// リアクションのチップは customEmojis を読んでいるので、差し替えに追従する。
	// 一覧が空 (初回や消去の後) のときだけ、絵文字が名前のまま出ないよう待つ。
	const pending = fetchCustomEmojis().catch(() => { /* empty */ });
	if (customEmojis.value.length === 0) await pending;
}

let cachedTags: string[] | null = null;
export function getCustomEmojiTags() {
	if (cachedTags) return cachedTags;

	const tags = new Set<string>();
	for (const emoji of customEmojis.value) {
		for (const tag of emoji.aliases) {
			tags.add(tag);
		}
	}
	const res = Array.from(tags);
	cachedTags = res;
	return res;
}
