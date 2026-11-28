// @ts-check

/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { createHash } from 'node:crypto';

/**
 * Hash of the built locales, embedded in the Service Worker as `_LOCALE_HASH_`.
 *
 * Each language is hashed as `JSON.stringify(locale)`, which is what i18n's
 * writeFrontendLocalesJson serves without its trailing `_version_`. Languages
 * are taken in sorted order, so the result does not depend on key order.
 *
 * @param {Record<string, unknown>} locales built locales (i18n's `build()`), keyed by language
 * @returns {string}
 */
export function localeHash(locales) {
	// Elythia: locale のキャッシュの名前に入れる (#4264)。配る JSON から同じ値を求め直せる
	// 形にしてあり、frontend の vitest がビルドした sw.js と配る locale を突き合わせる
	const hash = createHash('sha256');
	for (const lang of Object.keys(locales).sort()) {
		hash.update(lang).update('\0').update(JSON.stringify(locales[lang])).update('\0');
	}
	return hash.digest('hex').slice(0, 16);
}
