/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type { ILocale } from './types.js';

/**
 * Directory name, next to the upstream `locales/`, that holds Elythia's own
 * words for each language (`<lang>.yml`).
 */
export const ELYTHIA_LOCALES_DIR = 'locales-elythia';

/**
 * Lays Elythia's words over the upstream words of the same language.
 *
 * Nested sections are merged key by key. An empty string removes the
 * upstream word, so that the language falls back to the next one in the
 * chain. Neither argument is modified.
 */
export function overlayLocale<T extends ILocale>(upstream: T, own: ILocale | null | undefined): T {
	// 本家の語の並びを保ったまま、Elythia の語で上書き・追加する。
	// 本家の取り込みのたびに語の順序が入れ替わると、生成する型の差分が読めなくなるため
	const result: ILocale = { ...upstream };
	if (own == null) return result as T;

	// 節かどうかは型に頼らず値で確かめる (YAML の `~` は null になり、節としては扱えない)。
	// 文字列以外の値は scripts/check-locale-overrides.mjs が落とす
	for (const [key, value] of Object.entries(own) as [string, unknown][]) {
		const current: unknown = result[key];
		if (value === '') {
			// 本家の訳が Elythia の意味に合わないときに消して、上位の言語へ落とす
			delete result[key];
		} else if (isSection(value) && isSection(current)) {
			result[key] = overlayLocale(current, value);
		} else {
			result[key] = value as ILocale[string];
		}
	}
	return result as T;
}

function isSection(value: unknown): value is ILocale {
	return typeof value === 'object' && value !== null;
}
