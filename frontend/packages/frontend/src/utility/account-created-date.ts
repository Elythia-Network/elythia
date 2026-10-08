/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { versatileLang } from '@@/js/intl-const.js';

function isMidnightUtc(d: Date): boolean {
	return d.getUTCHours() === 0 && d.getUTCMinutes() === 0 && d.getUTCSeconds() === 0 && d.getUTCMilliseconds() === 0;
}

function dateFormat(locale: string, timeZone: string | undefined): Intl.DateTimeFormat {
	const options: Intl.DateTimeFormatOptions = { year: 'numeric', month: 'numeric', day: 'numeric', timeZone };
	try {
		return new Intl.DateTimeFormat(locale, options);
	} catch {
		return new Intl.DateTimeFormat('en-US', options);
	}
}

/**
 * Formats a remote account's creation time (`accountCreatedAt`) as a date
 * without the time of day.
 *
 * Mastodon の actor の `published` は日付の単位 (その日の 00:00Z) なので、時刻を
 * 出すと実際より細かく見え、UTC より西の地域では前日になる。00:00:00.000Z
 * ちょうどの値は UTC の日付で出し、それ以外 (Misskey の users/show など、
 * 時刻まで分かる値) は利用者の地域の日付で出す。
 */
export function accountCreatedDateString(iso: string, locale: string = versatileLang, timeZone?: string): string {
	const d = new Date(iso);
	return dateFormat(locale, isMidnightUtc(d) ? 'UTC' : timeZone).format(d);
}
