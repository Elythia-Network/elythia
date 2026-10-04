/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import * as Misskey from 'misskey-js';
import { version } from '@@/js/config.js';
import { languages } from 'i18n/const';
import type { Locale } from 'i18n';
import { acct } from '@/filters/user.js';
import { i18n } from '@/i18n.js';
import { instance } from '@/instance.js';

export const ABUSE_REPORT_COMMENT_MAX = 2048;

export const ABUSE_REPORT_LIMITS = {
	details: 1200,
	evidence: 400,
	when: 100,
	where: 300,
	renoteAppendMax: 200,
} as const;

export const ABUSE_REPORT_CATEGORIES = [
	'spam',
	'harassment',
	'impersonation',
	'illegal',
	'copyright',
	'doxxing',
	'other',
] as const;

export type AbuseReportCategory = typeof ABUSE_REPORT_CATEGORIES[number];

export interface AbuseReportContext {
	targetUsername: string;
	where?: string;
	when?: string;
	renoteSource?: { username: string; url: string };
	quoteSource?: { url: string };
}

export interface AbuseReportFormValues {
	category: AbuseReportCategory | '';
	details: string;
	when: string;
	where: string;
	evidence: string;
}

type AbuseReportFormLocale = Locale['_abuseReportForm'];

const localeCache = new Map<string, AbuseReportFormLocale>();

export function countRunes(str: string): number {
	return [...str].length;
}

export function matchLangCode(raw: string): string {
	if ((languages as readonly string[]).includes(raw)) return raw;
	const prefixed = (languages as readonly string[]).find(lang => lang.startsWith(`${raw}-`));
	if (prefixed) return prefixed;
	return 'ja-JP';
}

export function resolveInstanceLangCode(): string {
	return matchLangCode(instance.langs?.[0] ?? 'ja');
}

async function fetchLocaleJson(code: string): Promise<Locale | null> {
	try {
		const res = await window.fetch(`/assets/locales/${code}.${version}.json`);
		if (!res.ok) return null;
		return await res.json() as Locale;
	} catch {
		return null;
	}
}

export async function getAbuseReportFormLocale(): Promise<AbuseReportFormLocale> {
	const code = resolveInstanceLangCode();
	const cached = localeCache.get(code);
	if (cached) return cached;

	for (const tryCode of [code, 'ja-JP']) {
		const locale = await fetchLocaleJson(tryCode);
		const form = locale?._abuseReportForm;
		if (form) {
			localeCache.set(code, form);
			return form;
		}
	}

	// ponytail: 取得不能時は UI locale を最終フォールバック（送信不能よりマシ）
	const fallback = i18n.ts._abuseReportForm;
	localeCache.set(code, fallback);
	return fallback;
}

export function formatAbuseReportWhen(date: Date, langCode = resolveInstanceLangCode()): string {
	try {
		return new Intl.DateTimeFormat(langCode, {
			year: 'numeric',
			month: 'long',
			day: 'numeric',
			hour: 'numeric',
			minute: 'numeric',
		}).format(date);
	} catch {
		return new Intl.DateTimeFormat('ja-JP', {
			year: 'numeric',
			month: 'long',
			day: 'numeric',
			hour: 'numeric',
			minute: 'numeric',
		}).format(date);
	}
}

export function buildContextFromProfile(user: Misskey.entities.UserLite, baseUrl: string): AbuseReportContext {
	// **acct で組む。** username だけだとリモート利用者の該当 URL が自インスタンスの
	// 同名ユーザー (別人) を指すか 404 になり、対象ユーザー欄もどのインスタンスの
	// 誰か判別できない。
	return {
		targetUsername: acct(user),
		where: `${baseUrl}/@${acct(user)}`,
	};
}

function buildNoteWhere(note: Misskey.entities.Note, baseUrl: string): string {
	const localUrl = `${baseUrl}/notes/${note.id}`;
	const remoteUrl = note.url ?? note.uri;
	if (remoteUrl == null) return localUrl;
	// **改行を入れない。** 受け側は MkInput (single-line `<input>`) で、
	// HTML の value sanitization が LF を除去するため区切りが消えて URL が連結される。
	// ユーザーが 1 文字でも編集するとその連結済み文字列が送信される。
	return `${remoteUrl} (local: ${localUrl})`;
}

function noteHasOwnContent(note: Misskey.entities.Note): boolean {
	if (note.text != null && note.text !== '') return true;
	return (note.fileIds?.length ?? 0) > 0 || (note.files?.length ?? 0) > 0;
}

export function buildContextFromNote(note: Misskey.entities.Note, baseUrl: string): AbuseReportContext {
	const ctx: AbuseReportContext = {
		targetUsername: acct(note.user),
		where: buildNoteWhere(note, baseUrl),
		when: formatAbuseReportWhen(new Date(note.createdAt)),
	};

	if (note.renoteId) {
		const renote = note.renote;
		const renoteUrl = renote ? `${baseUrl}/notes/${renote.id}` : `${baseUrl}/notes/${note.renoteId}`;
		if (noteHasOwnContent(note)) {
			ctx.quoteSource = { url: renoteUrl };
		} else {
			ctx.renoteSource = {
				// **ここも acct で組む。** コメントは MkAbuseReport が <Mfm> で
				// レンダーするので、host を落とすと `@bob` が mention ノードになり
				// **ローカルの別人へリンクする**。該当 URL と同じ症状。
				username: renote?.user ? acct(renote.user) : '?',
				url: renoteUrl,
			};
		}
	}

	return ctx;
}

function appendLine(lines: string[], label: string, value: string | undefined): void {
	const trimmed = value?.trim();
	if (!trimmed) return;
	lines.push(`${label}${trimmed}`);
}

function buildRenoteLines(formLocale: AbuseReportFormLocale, context?: AbuseReportContext): string[] {
	const lines: string[] = [];
	if (context?.renoteSource) {
		const line = `${formLocale._commentLabel.renoteSource}@${context.renoteSource.username} / ${context.renoteSource.url}`;
		lines.push(truncateRunes(line, ABUSE_REPORT_LIMITS.renoteAppendMax));
	}
	if (context?.quoteSource) {
		const line = `${formLocale._commentLabel.quoteSource}${context.quoteSource.url}`;
		lines.push(truncateRunes(line, ABUSE_REPORT_LIMITS.renoteAppendMax));
	}
	return lines;
}

function truncateRunes(str: string, max: number): string {
	if (countRunes(str) <= max) return str;
	return [...str].slice(0, max).join('');
}

/** Joins the form values into the comment body **without** clamping. */
function joinAbuseReportLines(
	formLocale: AbuseReportFormLocale,
	values: AbuseReportFormValues,
	context: AbuseReportContext,
): string {
	const categoryLabel = values.category ? formLocale._category[values.category] : '';
	const lines: string[] = [];

	appendLine(lines, formLocale._commentLabel.category, categoryLabel);
	appendLine(lines, formLocale._commentLabel.targetUser, `@${context.targetUsername}`);
	appendLine(lines, formLocale._commentLabel.url, values.where.trim());
	appendLine(lines, formLocale._commentLabel.when, values.when.trim());
	appendLine(lines, formLocale._commentLabel.details, values.details.trim());
	appendLine(lines, formLocale._commentLabel.evidence, values.evidence.trim());
	lines.push(...buildRenoteLines(formLocale, context));

	return lines.join('\n');
}

/**
 * Builds the comment, clamping it to the API limit.
 *
 * **切り落としは最後の保険。** 検証系 (isAbuseReportFormValid /
 * remainingAbuseReportRunes) がこの戻り値を数えると、常に上限以下なので判定が
 * 恒真になり、自動収集した文脈 (リノート元など) が無言で欠ける。
 */
export function buildAbuseReportComment(
	formLocale: AbuseReportFormLocale,
	values: AbuseReportFormValues,
	context: AbuseReportContext,
): string {
	return truncateRunes(joinAbuseReportLines(formLocale, values, context), ABUSE_REPORT_COMMENT_MAX);
}

export function isAbuseReportFormValid(
	formLocale: AbuseReportFormLocale,
	values: AbuseReportFormValues,
	context: AbuseReportContext,
): boolean {
	if (!values.category || !values.details.trim()) return false;
	if (countRunes(values.details) > ABUSE_REPORT_LIMITS.details) return false;
	if (countRunes(values.evidence) > ABUSE_REPORT_LIMITS.evidence) return false;
	if (countRunes(values.when) > ABUSE_REPORT_LIMITS.when) return false;
	if (countRunes(values.where) > ABUSE_REPORT_LIMITS.where) return false;
	return countRunes(joinAbuseReportLines(formLocale, values, context)) <= ABUSE_REPORT_COMMENT_MAX;
}

export function remainingAbuseReportRunes(
	formLocale: AbuseReportFormLocale,
	values: AbuseReportFormValues,
	context: AbuseReportContext,
): number {
	return ABUSE_REPORT_COMMENT_MAX - countRunes(joinAbuseReportLines(formLocale, values, context));
}
