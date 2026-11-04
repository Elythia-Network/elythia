/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { shallowRef, watch } from 'vue';
import * as Misskey from 'misskey-js';
import { misskeyApi, misskeyApiGet } from '@/misskey-api.js';

function get(key: string) {
	const value = localStorage.getItem(key);
	if (value === null) return null;
	return JSON.parse(value);
}

function set(key: string, value: any) {
	localStorage.setItem(key, JSON.stringify(value));
}

const storageCache = await get('emojis');
export const customEmojis = shallowRef<Misskey.entities.EmojiSimple[]>(Array.isArray(storageCache) ? storageCache : []);

export const customEmojisMap = new Map<string, Misskey.entities.EmojiSimple>();
watch(customEmojis, emojis => {
	customEmojisMap.clear();
	for (const emoji of emojis) {
		customEmojisMap.set(emoji.name, emoji);
	}
}, { immediate: true });

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
 * Refreshes the custom emoji list while the embed boots.
 * Waits for the request only when there is no cached list.
 */
export async function refreshCustomEmojisOnBoot(): Promise<void> {
	// Elythia: 手元 (localStorage) に一覧があるときは、/api/emojis の応答を待たずに描く
	// (#3992、通常の画面の #3613 と同じ)。取得は裏で続け、返ってきたら customEmojis を
	// 差し替える。描いてある絵文字 (EmCustomEmoji) は customEmojis を読んでいるので追従する。
	// 一覧が空 (その埋め込み先での初回や消去の後) のときだけ、絵文字が名前のまま出ないよう待つ。
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
