/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { computed, reactive } from 'vue';
// Elythia: /api/meta の独自の項目 (mkGoVersion など) を型に持つ (#3418)
import type * as Elythia from 'elythia-js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { miLocalStorage } from '@/local-storage.js';

// TODO: 他のタブと永続化されたstateを同期

//#region loader
const providedMetaEl = window.document.getElementById('misskey_meta');

let cachedMeta = miLocalStorage.getItem('instance') ? JSON.parse(miLocalStorage.getItem('instance')!) : null;
let cachedAt = miLocalStorage.getItem('instanceCachedAt') ? parseInt(miLocalStorage.getItem('instanceCachedAt')!) : 0;
const providedMeta = providedMetaEl && providedMetaEl.textContent ? JSON.parse(providedMetaEl.textContent) : null;
const providedAt = providedMetaEl && providedMetaEl.dataset.generatedAt ? parseInt(providedMetaEl.dataset.generatedAt) : 0;
if (providedAt > cachedAt) {
	miLocalStorage.setItem('instance', JSON.stringify(providedMeta));
	miLocalStorage.setItem('instanceCachedAt', providedAt.toString());
	cachedMeta = providedMeta;
	cachedAt = providedAt;
}

// Elythia: 入口の画面が、HTMLに埋め込まれたmetaを取り直さずに使えるかの印 (#3967)。
// サーバーは要求ごとに /api/meta の全項目 (有効な広告を含む) を埋め込むので、このページの
// 読み込みで採ったものは /api/meta とほぼ同じ新しさを持つ (HTMLは public, max-age=30 なので、
// ブラウザやCDNのキャッシュから届いたときはその分だけ古い)。SPAの中で入口へ戻ってきたときは
// 古くなっているので、1回使ったら落とす
let providedMetaUnconsumed = providedMeta != null && cachedMeta === providedMeta;
//#endregion

// TODO: instanceをリアクティブにするかは再考の余地あり

export const instance: Elythia.MetaDetailed = reactive(cachedMeta ?? {});

export async function fetchInstance(force = false): Promise<Elythia.MetaDetailed> {
	if (!force) {
		const cachedAt = miLocalStorage.getItem('instanceCachedAt') ? parseInt(miLocalStorage.getItem('instanceCachedAt')!) : 0;

		if (Date.now() - cachedAt < 1000 * 60 * 60) {
			return instance;
		}
	}

	const meta = await misskeyApi('meta', {
		detail: true,
	});

	for (const [k, v] of Object.entries(meta)) {
		(instance[k as keyof typeof meta] as any) = v;
	}

	miLocalStorage.setItem('instance', JSON.stringify(instance));
	miLocalStorage.setItem('instanceCachedAt', Date.now().toString());

	return instance;
}

/**
 * Reports whether the instance info was taken from the meta embedded in this
 * page's HTML and has not been reported yet. Returns true at most once per page load.
 */
export function consumeProvidedMeta(): boolean {
	const result = providedMetaUnconsumed;
	providedMetaUnconsumed = false;
	return result;
}
