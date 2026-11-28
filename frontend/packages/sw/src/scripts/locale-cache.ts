/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Name of the CacheStorage cache that holds the locale JSON.
 *
 * version is the upstream version the locale URL carries, and localeHash
 * identifies the content of the built locales (Elythia's overlay included).
 */
export function localeCacheName(version: string, localeHash: string): string {
	// Elythia は本家の版を上げずに locales-elythia の語を変えるので、版だけの名前だと
	// 古い語を残したまま使い続ける。中身の hash を名前に入れ、変わったら activate で
	// 古いキャッシュを消させる (#4264)
	return `mk-cache-${version}-${localeHash}`;
}

/**
 * Options of the fetch that fills the locale cache.
 *
 * The locale URL keeps the same name while its content changes, so the
 * request revalidates instead of taking a heuristically fresh HTTP cache entry.
 */
export function localeFetchInit(signal: AbortSignal): RequestInit {
	return { signal, cache: 'no-cache' };
}
