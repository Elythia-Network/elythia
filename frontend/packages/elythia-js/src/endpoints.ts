/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type * as Misskey from 'misskey-js';
import type {
	AdminEmojiApplication,
	AdminSignupApplication,
	EmojiApplication,
	EmojiApplicationPendingLimit,
	EmojiApplicationQuotaReset,
	EmojiApplicationQuotaWindow,
	EmojiApplicationStatus,
	EmojiApplicationStatusCounts,
	FederationRule,
	FederationRuleBody,
	FederationRuleHit,
	RelatedEmojiApplication,
	SignupApplicationView,
} from './entities.js';

/**
 * The body of federation rules create and update.
 *
 * 省いた項目はゼロ値 (空文字列・0・空の配列・false・null) として読まれる。mode と target は
 * ゼロ値だと弾かれるので必須にしてある。
 */
type FederationRuleRequest = Partial<FederationRuleBody> & Pick<FederationRuleBody, 'mode' | 'target'>;

/**
 * Elythia-specific endpoints that misskey-js does not know about.
 *
 * 型を足したら pending-endpoints.txt から外す。キーと Go のルートは
 * internal/entitycompat の TestElythiaJS_EndpointsMatchRouter が突き合わせるので、
 * キーは `'<path>': {` の形で 1 行に書く。
 */
export type ElythiaEndpoints = {
	'admin/emoji-application/approve': {
		req: { applicationId: string };
		res: AdminEmojiApplication;
	};
	'admin/emoji-application/list': {
		req: {
			/** Defaults to `pending`. `processed` is approved, rejected and canceled. */
			filter?: 'all' | 'pending' | 'processed';
			/** Defaults to 30 (also when out of 1-100). */
			limit?: number;
			/** `null` is the same as omitting it. */
			untilId?: string | null;
		};
		res: AdminEmojiApplication[];
	};
	'admin/emoji-application/list-by-user': {
		req: {
			userId: string;
			/** Defaults to all. An unknown value is rejected. */
			status?: 'all' | EmojiApplicationStatus;
			query?: string;
			/** Defaults to 30 (also when out of 1-100). */
			limit?: number;
			/** `null` is the same as omitting it. */
			untilId?: string | null;
		};
		res: { items: AdminEmojiApplication[] };
	};
	'admin/emoji-application/reject': {
		req: {
			applicationId: string;
			reason?: string;
		};
		res: AdminEmojiApplication;
	};
	'admin/emoji-application/related': {
		req: {
			applicationId: string;
			/** Defaults to 10 (also when out of 1-100). */
			limit?: number;
			/** `null` is the same as omitting it. */
			untilId?: string | null;
		};
		res: {
			/** Counts every related application, not only the returned page. */
			counts: EmojiApplicationStatusCounts;
			items: RelatedEmojiApplication[];
		};
	};
	'admin/emoji-application/reset-user-quota': {
		req: {
			userId: string;
			/** Required; kept in the moderation log. */
			reason: string;
		};
		res: { lastReset: EmojiApplicationQuotaReset };
	};
	'admin/emoji-application/user-summary': {
		req: { userId: string };
		res: {
			counts: EmojiApplicationStatusCounts;
			windows: EmojiApplicationQuotaWindow[];
			pending: EmojiApplicationPendingLimit;
			/** `null` when the limit has never been reset, or the reset could not be looked up. */
			lastReset: EmojiApplicationQuotaReset | null;
		};
	};
	'admin/federation/rules/create': {
		req: FederationRuleRequest;
		res: FederationRule;
	};
	'admin/federation/rules/delete': {
		req: { ruleId: string };
		res: undefined;
	};
	'admin/federation/rules/hits': {
		req: {
			ruleId: string;
			/** Defaults to 50, the most kept (also when out of 1-50). */
			limit?: number;
		};
		/** Newest first. */
		res: FederationRuleHit[];
	};
	'admin/federation/rules/list': {
		req: Misskey.entities.EmptyRequest;
		/** In evaluation order. */
		res: FederationRule[];
	};
	'admin/federation/rules/update': {
		/** Replaces the whole rule; an omitted field is cleared, not kept. */
		req: FederationRuleRequest & { ruleId: string };
		res: FederationRule;
	};
	'admin/signup-application/approve': {
		req: { applicationId: string };
		res: { ok: true };
	};
	'admin/signup-application/list': {
		req: {
			/** Defaults to `all`. `processed` is rejected, expired and completed. */
			filter?: 'all' | 'pending' | 'approved' | 'processed';
			/** Defaults to 30, capped at 100. */
			limit?: number;
			offset?: number;
		};
		res: {
			applications: AdminSignupApplication[];
			/** The number of applications that match the filter. */
			count: number;
		};
	};
	'admin/signup-application/reject': {
		req: { applicationId: string };
		res: { ok: true };
	};
	'emoji-application/cancel': {
		req: { applicationId: string };
		res: undefined;
	};
	'emoji-application/create': {
		req: {
			name: string;
			/** Defaults to `own`. */
			kind?: 'own' | 'remote';
			/** Required when `kind` is `own`. */
			license?: string;
			comment?: string;
			/** Required when `kind` is `own`. */
			fileId?: string;
			category?: string;
			aliases?: string[];
			isSensitive?: boolean;
			/** Required when `kind` is `remote`. */
			remoteHost?: string;
			/** Required when `kind` is `remote`. */
			remoteName?: string;
		};
		res: EmojiApplication;
	};
	'emoji-application/list-mine': {
		req: {
			/** Defaults to 30 (also when out of 1-100). */
			limit?: number;
			/** `null` is the same as omitting it. */
			untilId?: string | null;
		};
		res: EmojiApplication[];
	};
	'signup-application/apply': {
		req: {
			/** One value per form field, in the order of `meta.signupApplicationForm`. */
			answers: string[];
			/** Required only while no real captcha provider is enabled. */
			formToken?: string;
			'hcaptcha-response'?: string | null;
			'g-recaptcha-response'?: string | null;
			'turnstile-response'?: string | null;
			'm-captcha-response'?: string | null;
			'testcaptcha-response'?: string | null;
		};
		res: {
			/** Shown only here; the server keeps just its hash. */
			claimCode: string;
			application: SignupApplicationView;
		};
	};
	'signup-application/form-token': {
		req: Misskey.entities.EmptyRequest;
		res: {
			/** An empty string when the server does not require a form token. */
			token: string;
			minWaitSeconds: number;
		};
	};
	'signup-application/register': {
		req: {
			claimCode: string;
			username: string;
			password: string;
			/** Required when `meta.emailRequiredForSignup` is set. */
			emailAddress?: string;
		};
		/**
		 * The new account, or `undefined` (204) when a confirmation email was sent
		 * instead.
		 */
		res: Misskey.entities.SignupResponse | undefined;
	};
	'signup-application/status': {
		req: { claimCode: string };
		res: { application: SignupApplicationView };
	};
};
