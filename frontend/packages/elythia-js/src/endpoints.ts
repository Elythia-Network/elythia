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
	DatabaseHealthReport,
	DeliveryOutcomeClass,
	DriveUsage,
	EmojiApplication,
	EmojiApplicationPendingLimit,
	EmojiApplicationQuotaReset,
	EmojiApplicationQuotaWindow,
	EmojiApplicationStatus,
	EmojiApplicationStatusCounts,
	FederationHealthReport,
	FederationRule,
	FederationRuleBody,
	FederationRuleHit,
	GoneInstance,
	GoneInstanceCleanResult,
	InboxOutcomeClass,
	IPAccountsResult,
	IPLookupLogResult,
	IPRelatedAccountsResult,
	JoinedChatRoom,
	LegacySigninResponse,
	RelatedEmojiApplication,
	RemoteCheckReport,
	RemoteEmojiMeta,
	RoleAssignmentLookup,
	SelfCheckReport,
	ServerMetrics,
	ServerPluginInfo,
	SignupApplicationView,
	SilentFollow,
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
	'admin/database-health': {
		req: Misskey.entities.EmptyRequest;
		res: DatabaseHealthReport;
	};
	'admin/drive/usage': {
		req: {
			/** Ignores the cached snapshot and aggregates again. */
			forceRecalc?: boolean;
		};
		res: DriveUsage;
	};
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
	'admin/emoji/fetch-remote-meta': {
		/** Either `emojiId`, or both `name` and `host`. `emojiId` wins when given. */
		req:
			| { emojiId: string; name?: string; host?: string }
			| { emojiId?: string; name: string; host: string };
		/**
		 * Failing to fetch from the origin is not an error: it returns
		 * `fetched: false` with the stored emoji.
		 */
		res: RemoteEmojiMeta;
	};
	'admin/federation/check-host': {
		req: {
			/** A host or a URL. This server itself is rejected (use `admin/self-check`). */
			host: string;
			/**
			 * `user`, `@user`, `user@host` or `@user@host`. A different host than
			 * `host` is rejected with `INVALID_ACCOUNT`.
			 */
			account?: string;
		};
		res: RemoteCheckReport;
	};
	'admin/federation/clean-gone-instance': {
		/** A host or a URL of a goneSuspended instance. */
		req: { host: string };
		res: GoneInstanceCleanResult;
	};
	'admin/federation/close-delivery-breaker': {
		/** A host or a URL. */
		req: { host: string };
		res: undefined;
	};
	'admin/federation/delivery-health': {
		req: {
			/** Defaults to 3600 (also when 0 or less), capped at 3600. Aggregated in whole minutes. */
			windowSeconds?: number;
		};
		res: FederationHealthReport<DeliveryOutcomeClass>;
	};
	'admin/federation/gone-instances': {
		req: Misskey.entities.EmptyRequest;
		res: GoneInstance[];
	};
	'admin/federation/inbox-health': {
		req: {
			/** Defaults to 3600 (also when 0 or less), capped at 3600. Aggregated in whole minutes. */
			windowSeconds?: number;
		};
		res: FederationHealthReport<InboxOutcomeClass>;
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
	'admin/ip/accounts': {
		req: {
			/** Normalized before the search; a value that is not an IP is rejected. */
			ip: string;
			/** Defaults to the retention period (90). Outside 1-3650 is rejected. */
			sinceDays?: number;
			/** Defaults to 30. Outside 1-100 is rejected. */
			limit?: number;
			/** Defaults to 0. Outside 0-10000 is rejected. */
			offset?: number;
		};
		res: IPAccountsResult;
	};
	'admin/ip/lookup-log': {
		req: {
			/** Defaults to 30. Outside 1-100 is rejected. */
			limit?: number;
			/** Defaults to 0. Outside 0-10000 is rejected. */
			offset?: number;
		};
		res: IPLookupLogResult;
	};
	'admin/ip/related-accounts': {
		req: {
			/**
			 * A local user. Remote users are rejected. `ACCESS_DENIED` is returned for
			 * the root user and system accounts, for administrators other than the
			 * caller, and for other moderators unless the caller is an administrator.
			 * A failure to determine the roles is a 500.
			 */
			userId: string;
			/** Defaults to the retention period (90). Outside 1-3650 is rejected. */
			sinceDays?: number;
			/** Defaults to 30. Outside 1-100 is rejected. */
			limit?: number;
			/** Defaults to 0. Outside 0-10000 is rejected. */
			offset?: number;
		};
		res: IPRelatedAccountsResult;
	};
	'admin/roles/assignment-show': {
		req: {
			userId: string;
			roleId: string;
		};
		res: RoleAssignmentLookup;
	};
	'admin/self-check': {
		req: Misskey.entities.EmptyRequest;
		res: SelfCheckReport;
	};
	'admin/server-metrics': {
		req: Misskey.entities.EmptyRequest;
		res: ServerMetrics;
	};
	'admin/server-plugins': {
		req: Misskey.entities.EmptyRequest;
		res: {
			plugins: ServerPluginInfo[];
			/**
			 * Plugin schemas that no compiled-in plugin owns. `null` when they could
			 * not be checked (not the same as none).
			 */
			orphanSchemas: string[] | null;
		};
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
	'chat/messages': {
		/**
		 * `roomId` wins when both are given; with neither, the result is empty.
		 * Unlike `chat/messages/user-timeline` and `room-timeline`, it takes no
		 * cursor.
		 */
		req: {
			/** The caller must be the owner or a member of the room, or a moderator. */
			roomId?: string;
			/** The other user of a one-to-one conversation with the caller. */
			userId?: string;
			/** Defaults to 20 (also when 0 or less), capped at 100. */
			limit?: number;
		};
		/** Newest first. */
		res: Misskey.entities.ChatMessage[];
	};
	'chat/messages/create': {
		/**
		 * Takes the request of `chat/messages/create-to-user` or
		 * `chat/messages/create-to-room` (the same handler serves all three).
		 * `toRoomId` wins when both are given.
		 */
		req: ({ toUserId: string } | { toRoomId: string }) & {
			/** Up to 2000 characters. Either `text` (an empty string counts) or `fileId` is required. */
			text?: string | null;
			/** A drive file of the caller. */
			fileId?: string | null;
		};
		res: Misskey.entities.ChatMessage;
	};
	'chat/messages/reactions/create': {
		/** The same handler as `chat/messages/react`. */
		req: Misskey.entities.ChatMessagesReactRequest;
		res: undefined;
	};
	'chat/messages/reactions/delete': {
		/** The same handler as `chat/messages/unreact`. */
		req: Misskey.entities.ChatMessagesUnreactRequest;
		res: undefined;
	};
	'chat/messages/read': {
		/** Marks one message as read. Only a participant of the message may do so. */
		req: { messageId: string };
		res: undefined;
	};
	'chat/messages/update': {
		/** Only the sender may edit the message. */
		req: {
			messageId: string;
			/** The new text. Omitting it (or `null`) clears the text to an empty string. */
			text?: string | null;
		};
		res: undefined;
	};
	'chat/rooms/invitations/accept': {
		// Go 側は invitationId も読むが使っていない。招待は (呼び出した人, roomId) で
		// 引くので、型には載せない
		req: { roomId: string };
		res: undefined;
	};
	'chat/rooms/invitations/delete': {
		/** Withdraws an invitation. Only the owner of the room may do so. */
		req: { invitationId: string };
		res: undefined;
	};
	'chat/rooms/invitations/reject': {
		req: { roomId: string };
		res: undefined;
	};
	'chat/rooms/joined': {
		/** The rooms the caller is a member of (not the ones the caller owns). */
		req: {
			/** Defaults to 30 (also when 0 or less), capped at 100. */
			limit?: number;
			sinceId?: string;
			untilId?: string;
			sinceDate?: number;
			untilDate?: number;
		};
		/**
		 * Newest room (by room id, not by when the caller joined) first, unless only
		 * `sinceId` / `sinceDate` is given (then oldest first).
		 */
		res: JoinedChatRoom[];
	};
	'chat/rooms/members/ban': {
		/** Removes a member from the room. Only the owner of the room may do so. */
		req: {
			roomId: string;
			userId: string;
		};
		res: undefined;
	};
	'chat/rooms/members/update-membership': {
		/** Only the owner of the room may do so. */
		req: {
			roomId: string;
			userId: string;
			/** Left unchanged when omitted. */
			isMuted?: boolean;
		};
		res: undefined;
	};
	'chat/rooms/transfer-ownership': {
		req: {
			roomId: string;
			/** A local user who is already a member of the room. */
			userId: string;
		};
		res: undefined;
	};
	'chat/rooms/unmute': {
		/** The same as `chat/rooms/mute` with `mute: false`, except that it returns 204 whenever the membership cannot be found (a non-member, or a lookup failure). */
		req: { roomId: string };
		res: undefined;
	};
	'chat/unread-count': {
		req: Misskey.entities.EmptyRequest;
		/** The number of unread one-to-one messages to the caller (room messages are not counted). */
		res: { count: number };
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
	'following/silent/list': {
		/**
		 * The follows toward the caller that succeeded without a notification
		 * because of `followApprovalAction: 'silentFollow'` (#3466), newest first.
		 */
		req: {
			/** From 1 to 100. Defaults to 10. */
			limit?: number;
			sinceId?: string;
			untilId?: string;
			sinceDate?: number;
			untilDate?: number;
		};
		res: SilentFollow[];
	};
	'i/flashs': {
		/** The same handler as `flash/my`. */
		req: Misskey.entities.FlashMyRequest;
		res: Misskey.entities.FlashMyResponse;
	};
	'i/flashs/likes': {
		/** The same handler as `flash/my-likes`. */
		req: Misskey.entities.FlashMyLikesRequest;
		res: Misskey.entities.FlashMyLikesResponse;
	};
	'roles/assignment-show': {
		/**
		 * Whether the caller has an exact assignment of the role. A private role
		 * the caller is not assigned is answered as `NO_SUCH_ROLE`.
		 */
		req: { roleId: string };
		res: RoleAssignmentLookup;
	};
	'signin': {
		/**
		 * The legacy sign-in, kept for old clients. Use `signin-flow` instead: this
		 * one cannot complete two-factor authentication.
		 */
		req: {
			username: string;
			/** Omit it to ask for the next step. */
			password?: string;
			/** Checked only when the user has two-factor authentication disabled. */
			'hcaptcha-response'?: string | null;
			'g-recaptcha-response'?: string | null;
			'turnstile-response'?: string | null;
			'm-captcha-response'?: string | null;
			'testcaptcha-response'?: string | null;
		};
		res: LegacySigninResponse;
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
