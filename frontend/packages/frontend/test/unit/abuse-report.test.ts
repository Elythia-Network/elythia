/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, it } from 'vitest';
import type { ParameterizedString } from 'i18n';
import {
	ABUSE_REPORT_COMMENT_MAX,
	buildAbuseReportComment,
	buildContextFromNote,
	buildContextFromProfile,
	countRunes,
	isAbuseReportFormValid,
	remainingAbuseReportRunes,
	matchLangCode,
	type AbuseReportFormValues,
} from '@/utility/abuse-report.js';

const formLocale = {
	category: '違反カテゴリ',
	categoryRequired: '',
	targetUser: '対象ユーザー',
	url: '該当URL',
	when: '発生日時',
	details: '詳細',
	detailsRequired: '',
	detailsCaption: '',
	evidence: '補足・証拠',
	evidenceCaption: '',
	renoteSource: 'リノート元',
	quoteSource: '引用元ノート',
	remainingChars: '残り {n} 文字' as ParameterizedString<'n'>,
	totalTooLong: '通報内容が長すぎます（{max} 文字以内）' as ParameterizedString<'max'>,
	_category: {
		spam: 'スパム',
		harassment: '嫌がらせ・迷惑行為',
		impersonation: 'なりすまし',
		illegal: '違法・危険なコンテンツ',
		copyright: '著作権侵害',
		doxxing: '個人情報の晒し',
		other: 'その他',
	},
	_commentLabel: {
		category: '【違反カテゴリ】',
		targetUser: '【対象ユーザー】',
		url: '【該当URL】',
		when: '【発生日時】',
		details: '【詳細】',
		evidence: '【補足・証拠】',
		renoteSource: '【リノート元】',
		quoteSource: '【引用元ノート】',
	},
};

const baseValues: AbuseReportFormValues = {
	category: 'spam',
	details: '迷惑行為の詳細',
	when: '2026年9月5日 12:34',
	where: 'https://example.com/notes/abc',
	evidence: '',
};

const baseContext = {
	targetUsername: 'alice',
	where: 'https://example.com/notes/abc',
	when: '2026年9月5日 12:34',
};

describe('abuse-report', () => {
	it('builds structured comment with required fields', () => {
		const comment = buildAbuseReportComment(formLocale, baseValues, baseContext);
		expect(comment).toContain('【違反カテゴリ】スパム');
		expect(comment).toContain('【対象ユーザー】@alice');
		expect(comment).toContain('【詳細】迷惑行為の詳細');
		expect(countRunes(comment)).toBeLessThanOrEqual(ABUSE_REPORT_COMMENT_MAX);
	});

	it('omits empty optional lines', () => {
		const comment = buildAbuseReportComment(formLocale, {
			...baseValues,
			when: '',
			where: '',
			evidence: '',
		}, { targetUsername: 'alice' });
		expect(comment).not.toContain('【該当URL】');
		expect(comment).not.toContain('【発生日時】');
		expect(comment).not.toContain('【補足・証拠】');
	});

	it('appends renote and quote context lines', () => {
		const comment = buildAbuseReportComment(formLocale, baseValues, {
			...baseContext,
			renoteSource: { username: 'bob', url: 'https://example.com/notes/src' },
			quoteSource: { url: 'https://example.com/notes/quote' },
		});
		expect(comment).toContain('【リノート元】@bob / https://example.com/notes/src');
		expect(comment).toContain('【引用元ノート】https://example.com/notes/quote');
	});

	it('rejects invalid form values', () => {
		expect(isAbuseReportFormValid(formLocale, { ...baseValues, category: '' }, baseContext)).toBe(false);
		expect(isAbuseReportFormValid(formLocale, { ...baseValues, details: '' }, baseContext)).toBe(false);
	});

	it('builds note context for quote renote', () => {
		const ctx = buildContextFromNote({
			id: 'n1',
			createdAt: '2026-09-05T03:34:56.000Z',
			text: 'quote text',
			renoteId: 'n0',
			renote: {
				id: 'n0',
				user: { username: 'bob' },
			},
			user: { username: 'alice' },
		} as never, 'https://example.com');
		expect(ctx.where).toContain('https://example.com/notes/n1');
		expect(ctx.quoteSource?.url).toBe('https://example.com/notes/n0');
		expect(ctx.renoteSource).toBeUndefined();
	});

	it('includes remote note URL in where', () => {
		const ctx = buildContextFromNote({
			id: 'n1',
			createdAt: '2026-09-05T03:34:56.000Z',
			url: 'https://remote.example/notes/orig',
			user: { username: 'alice' },
		} as never, 'https://example.com');
		// **単一行で持つ。** 受け側が MkInput (`<input>`) なので改行は消える。
		expect(ctx.where).toBe('https://remote.example/notes/orig (local: https://example.com/notes/n1)');
	});

	it('treats file-only renote as quote', () => {
		const ctx = buildContextFromNote({
			id: 'n1',
			createdAt: '2026-09-05T03:34:56.000Z',
			text: '',
			fileIds: ['f1'],
			renoteId: 'n0',
			renote: { id: 'n0', user: { username: 'bob' } },
			user: { username: 'alice' },
		} as never, 'https://example.com');
		expect(ctx.quoteSource?.url).toBe('https://example.com/notes/n0');
		expect(ctx.renoteSource).toBeUndefined();
	});

	it('resolves de/fr/ko lang codes', () => {
		expect(matchLangCode('de')).toBe('de-DE');
		expect(matchLangCode('fr')).toBe('fr-FR');
		expect(matchLangCode('ko')).toBe('ko-KR');
		expect(matchLangCode('ja-JP')).toBe('ja-JP');
	});

	// **上限の判定は切り落とし前を見る。** buildAbuseReportComment は末尾で
	// truncate するので、その戻り値を数えると判定が恒真になり、自動収集した
	// 文脈 (リノート元) が無言で欠ける。
	it('rejects a form whose joined comment exceeds the limit', () => {
		const values: AbuseReportFormValues = {
			category: 'spam',
			where: 'w'.repeat(300),
			when: 'n'.repeat(100),
			details: 'd'.repeat(1200),
			evidence: 'e'.repeat(400),
		};
		// **フィールド名は AbuseReportContext に合わせる。** 変数経由だと TypeScript の
		// excess property check が効かず、誤った名前でも型検査を通ってしまう。
		// `renoteSource` でないと buildRenoteLines が何も足さず、
		// 「捨てられる末尾がリノート元の行」という状況を再現できない。
		const context = {
			targetUsername: 'alice',
			renoteSource: { username: 'someone@remote.example', url: 'https://example.com/notes/9abcdefghijklmno' },
		};

		expect(countRunes(buildAbuseReportComment(formLocale, values, context)))
			.toBeLessThanOrEqual(ABUSE_REPORT_COMMENT_MAX);
		expect(isAbuseReportFormValid(formLocale, values, context)).toBe(false);
		expect(remainingAbuseReportRunes(formLocale, values, context)).toBeLessThan(0);
	});

	// リモート利用者は acct (username@host) で組む。username だけだと該当 URL が
	// 自インスタンスの同名ユーザー (別人) を指すか 404 になる。
	it('keeps the host for remote users', () => {
		const remote = { id: 'u1', username: 'alice', host: 'remote.example' } as never;
		const ctx = buildContextFromProfile(remote, 'https://example.com');
		expect(ctx.targetUsername).toBe('alice@remote.example');
		expect(ctx.where).toBe('https://example.com/@alice@remote.example');

		const local = { id: 'u2', username: 'bob', host: null } as never;
		const localCtx = buildContextFromProfile(local, 'https://example.com');
		expect(localCtx.targetUsername).toBe('bob');
		expect(localCtx.where).toBe('https://example.com/@bob');
	});

	// where は single-line <input> に入るので改行を含めない。含めると HTML の
	// value sanitization で LF が消え、URL がセパレータ無しで連結される。
	it('keeps where on a single line for remote notes', () => {
		const note = {
			id: 'n1',
			createdAt: new Date().toISOString(),
			user: { id: 'u1', username: 'alice', host: 'remote.example' },
			url: 'https://remote.example/notes/orig',
			text: 'hi',
		} as never;
		const ctx = buildContextFromNote(note, 'https://example.com');
		// note 側も host を保つ (profile 側だけ守られていた)。
		expect(ctx.targetUsername).toBe('alice@remote.example');
		expect(ctx.where).not.toContain('\n');
		expect(ctx.where).toContain('https://remote.example/notes/orig');
		expect(ctx.where).toContain('https://example.com/notes/n1');
	});

	// リノート元の作者も acct で組む。**コメントは <Mfm> でレンダーされる**ので、
	// host を落とすと `@bob` が mention ノードになりローカルの別人へリンクする。
	it('keeps the host for the renote source author', () => {
		const note = {
			id: 'n1',
			createdAt: new Date().toISOString(),
			user: { id: 'u1', username: 'alice', host: null },
			renoteId: 'r1',
			renote: { id: 'r1', user: { id: 'u2', username: 'bob', host: 'remote.example' } },
		} as never;
		const ctx = buildContextFromNote(note, 'https://example.com');
		expect(ctx.renoteSource?.username).toBe('bob@remote.example');
	});
});
