/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { shallowRef } from 'vue';
import type { ShallowRef } from 'vue';
import type * as Elythia from 'elythia-js';
import { consumeProvidedMeta, fetchInstance, instance } from '@/instance.js';

/**
 * Instance info for the entrance page (`/`).
 *
 * Holds the already loaded instance info right away instead of waiting for
 * `/api/meta`. It is refetched only when it may be stale (not the meta embedded
 * in this page's HTML), and then in the background unless nothing is loaded yet.
 */
export function useWelcomeInstance(): ShallowRef<Elythia.MetaDetailed | null> {
	// 本家は fetchInstance(true) を待ってから描くので、1往復ぶん描画が遅れる(#3967)。
	// HTMLに埋め込まれたmetaは要求ごとに作られ、/api/meta の全項目 (広告を含む) を持つので
	// 取り直さない。HTMLのキャッシュ (public, max-age=30) の分だけ古いことはある
	const embedded = consumeProvidedMeta();
	const loaded = Object.keys(instance).length > 0;
	const result = shallowRef<Elythia.MetaDetailed | null>(loaded ? instance : null);
	if (!embedded || !loaded) {
		// SPAの中で戻ってきたときや、手元のキャッシュを採ったときは古いことがある。
		// fetchInstance は同じ instance を書き換えるので、描いた画面もそのまま追従する
		fetchInstance(true).then((res) => {
			result.value = res;
		});
	}
	return result;
}
