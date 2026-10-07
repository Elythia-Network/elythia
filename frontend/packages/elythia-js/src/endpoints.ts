/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type * as Misskey from 'misskey-js';
import type {
	AdminEmojiApplication,
	AdminSignupApplication,
	BubbleGameMode,
	BubbleVersusMatch,
	BubbleVersusRecord,
	BubbleVersusReportReason,
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
	'bubble-game/versus/accept': {
		req: { matchId: string };
		res: BubbleVersusMatch;
	};
	'bubble-game/versus/cancel': {
		/**
		 * Withdraws an invitation the caller sent, or calls off an accepted match
		 * before it starts (either player).
		 */
		req: { matchId: string };
		res: undefined;
	};
	'bubble-game/versus/decline': {
		req: { matchId: string };
		res: undefined;
	};
	'bubble-game/versus/history': {
		req: {
			/**
			 * Defaults to the caller. Records of others are shown only when both
			 * players made them public.
			 */
			userId?: string;
			/** Defaults to 10. Outside 1-100 is rejected. */
			limit?: number;
			untilId?: string;
			untilDate?: number;
		};
		/** Newest first. */
		res: BubbleVersusRecord[];
	};
	'bubble-game/versus/invitations': {
		req: Misskey.entities.EmptyRequest;
		/** The invitations the caller received and has not answered yet. */
		res: BubbleVersusMatch[];
	};
	'bubble-game/versus/invite': {
		req: {
			/** A local user other than the caller. */
			userId: string;
			gameMode: BubbleGameMode;
		};
		/** An unanswered invitation to the same user in the same mode is returned as is. */
		res: BubbleVersusMatch;
	};
	'bubble-game/versus/record': {
		req: { matchId: string };
		/** Includes `seed` and the logs, for the replay. */
		res: BubbleVersusRecord;
	};
	'bubble-game/versus/report': {
		req: {
			matchId: string;
			score: number;
			frame: number;
			reason: BubbleVersusReportReason;
			logs: number[][];
			/** The engine version the logs were made with. Older clients omit it. */
			gameVersion?: number;
		};
		res: BubbleVersusMatch;
	};
	'bubble-game/versus/set-public': {
		req: {
			matchId: string;
			isPublic: boolean;
		};
		res: undefined;
	};
	'bubble-game/versus/show': {
		req: { matchId: string };
		res: BubbleVersusMatch;
	};
	'drive/files/create-chunked/abort': {
		req: { uploadId: string };
		res: undefined;
	};
	'drive/files/create-chunked/append': {
		/** Sent as multipart/form-data, like `drive/files/create`. */
		req: {
			uploadId: string;
			/**
			 * The chunk's position, from 0. Chunks must be sent in order; resending
			 * an accepted index with the same content succeeds again.
			 */
			index: number;
			/**
			 * Exactly the session's `chunkSize` bytes, except the last chunk, which
			 * carries the (non-empty) remainder.
			 */
			chunk: Blob;
		};
		res: {
			index: number;
			/** The index the server expects next. */
			next: number;
			receivedBytes: number;
			totalSize: number;
			completed: boolean;
		};
	};
	'drive/files/create-chunked/finish': {
		req: { uploadId: string };
		/** The same shape as `drive/files/create`. */
		res: Misskey.entities.DriveFile;
	};
	'drive/files/create-chunked/start': {
		req: {
			/** Trimmed; an empty name or `blob` is then stored as `untitled`. */
			name?: string;
			/** The total size in bytes, checked against the size and capacity limits up front. */
			size: number;
			comment?: string | null;
			folderId?: string | null;
			isSensitive?: boolean;
			force?: boolean;
		};
		res: {
			uploadId: string;
			/** Fixed for the session, even if the server setting changes later. */
			chunkSize: number;
			totalChunks: number;
			expiresAt: string;
		};
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
