/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type * as Misskey from 'misskey-js';
import type { FederationInstance, FederationStatus, FollowApprovalAction, Page, PageVisibility, SignupApplicationFormField, SuspendedSoftware } from './entities.js';

type WithFederationStatus<T> = T extends unknown[]
	? WithFederationStatus<T[number]>[]
	: T extends object ? T & { federationStatus?: FederationStatus } : T;

/**
 * Request fields that Elythia adds to upstream endpoints, keyed by the endpoint.
 *
 * `Endpoints` では本家の req にこれを交差で重ねる。本家のエンドポイントの意味は変えず、
 * 項目を足すだけ (CLAUDE.md の冒頭の方針) なので、ここに書くのは省略できる項目に限る。
 * キーは本家にあるエンドポイントだけにする (index.ts の型が弾く)。独自のエンドポイントは
 * endpoints.ts の ElythiaEndpoints に書く。こちらは Go のルートとの突き合わせ
 * (internal/entitycompat) の対象ではない。
 */
export type ElythiaRequestExtensions = {
	'signup': {
		/**
		 * Sets the initial federation state. Omitted values default to `on`;
		 * official signup sends `pending` for the first-run tutorial. An account
		 * that has been referenced externally cannot be switched back to `off`.
		 */
		federationStatus?: FederationStatus;
	};
	'admin/accounts/create': {
		/**
		 * Federation state for the newly created account. Defaults to `on`.
		 * An account that has been referenced externally cannot be switched back
		 * to `off`.
		 */
		federationStatus?: FederationStatus;
	};
	'admin/abuse-user-reports': {
		/**
		 * Filters by whether the report is resolved. Read only when `state` is
		 * `all`, empty or `null` (it predates `state`).
		 */
		resolved?: boolean | null;
		/** Returns only this report, for the link of the abuse report notification (#2868). */
		reportId?: string;
	};
	'admin/drive/files': {
		/** The host of the files. Wins over `hostname`. */
		host?: string | null;
	};
	'admin/emoji/copy': {
		/**
		 * Overrides the name of the copied emoji (`^[a-zA-Z0-9_]+$`, up to 128
		 * characters). Upstream copies the remote name as is.
		 */
		name?: string | null;
		/** Overrides the category. `null` is the same as omitting it. */
		category?: string | null;
		/** Overrides the aliases. `null` is the same as omitting it. */
		aliases?: string[] | null;
		/** Overrides the license. `null` is the same as omitting it. */
		license?: string | null;
		/** Overrides the sensitive flag. `null` is the same as omitting it. */
		isSensitive?: boolean | null;
	};
	'admin/emoji/list': {
		/** Filters by the exact category (#3857). */
		category?: string | null;
	};
	'admin/queue/jobs': {
		/**
		 * The jobs of a page, from 1 to 100. Giving `limit` or `page` pages the
		 * jobs instead of listing them per state like upstream (#3167).
		 */
		limit?: number;
		/** The page, from 1. */
		page?: number;
	};
	'admin/show-user': {
		/** `false` leaves out the sign-in history. `null` is the same as omitting it. */
		withSignins?: boolean | null;
	};
	'admin/update-meta': {
		/** Accepts signups only through applications that a moderator approves. */
		approvalRequiredForSignup?: boolean;
		/**
		 * Accepts no registrations at all, not even with an invitation code.
		 * Wins over the other registration modes and keeps
		 * `approvalRequiredForSignup` as is.
		 */
		registrationClosed?: boolean;
		/** The application form. `null` is the same as an empty array. */
		signupApplicationForm?: SignupApplicationFormField[] | null;
		/** The minimum length of a local username, from 1 to 20. */
		minimumUsernameLength?: number;
		chunkedUploadEnabled?: boolean;
		/** The size of one chunk in MiB, from 5 to 32. */
		chunkedUploadChunkSizeMb?: number;
		/** From 5 to 1440. */
		chunkedUploadSessionTtlMinutes?: number;
		/** From 1 to 2147483647. */
		chunkedUploadMaxSessionsPerUser?: number;
		/** From 1 to 2147483647. */
		chunkedUploadMaxPendingMbPerUser?: number;
		/** Periodically deletes remote users that only relays brought in. */
		enableRelayOrphanUserCleanup?: boolean;
		/** Days after a relay-only user was last seen, from 7 to 3650. */
		relayOrphanUserGraceDays?: number;
		/** Keeps notes seen only through relays in Redis instead of the database. */
		enableEphemeralRelayNotes?: boolean;
		/** Minutes such a note is kept, from 1 to 525600. */
		ephemeralRelayNoteTtlMinutes?: number;
		/**
		 * Software whose activities the inbox refuses (#3362). Matched by the same
		 * rules as `deliverSuspendedSoftware`.
		 */
		inboxSuspendedSoftware?: SuspendedSoftware[];
	};
	'antennas/update': {
		/**
		 * Sets whether the antenna is active. When omitted (or `null`), the
		 * update makes it active again, as upstream does.
		 */
		isActive?: boolean | null;
	};
	'chat/history': {
		/**
		 * Returns only the conversations whose latest message is older than this
		 * id: pass the id of the last item of the previous page (#3653).
		 * Upstream cannot read past `limit` conversations.
		 */
		untilId?: string;
	};
	'i/update': {
		/**
		 * How a follow from an account younger than `followApprovalLocalSeconds` /
		 * `followApprovalRemoteSeconds` is handled. Elythia-only.
		 */
		followApprovalAction?: FollowApprovalAction;
	};
	'federation/instances': {
		/** The cursor of the paginator (#493). */
		sinceId?: string;
		/** The cursor of the paginator (#493). */
		untilId?: string;
		/** The cursor by date, in milliseconds (#1173). */
		sinceDate?: number;
		/** The cursor by date, in milliseconds (#1173). */
		untilDate?: number;
	};
	'i/move': {
		/** The password of the account, checked when given (#1546). */
		password?: string | null;
	};
	'i/regenerate-token': {
		/** The two-factor authentication code. Required when two-factor authentication is enabled. */
		token?: string | null;
	};
	'notes/global-timeline': {
		/**
		 * Returns fewer notes than `limit` instead of reading on. Upstream has it on
		 * the other timelines only.
		 */
		allowPartial?: boolean;
	};
	'notes': {
		/**
		 * Returns these notes (up to 100) instead of the public notes, like
		 * `notes/show-partial-bulk`.
		 */
		noteIds?: string[];
	};
	'pages/create': {
		/** Who can see the Page (#3479). Defaults to `public`. */
		visibility?: PageVisibility;
	};
	'pages/update': {
		/** Who can see the Page (#3479). Omitting it (or `null`) keeps the current value. */
		visibility?: PageVisibility | null;
	};
};

/**
 * Responses of upstream endpoints whose misskey-js type is wrong, keyed by the
 * endpoint. `Endpoints` replaces the upstream res with these.
 *
 * misskey-js の型は本家の API 定義から生成されるので、定義が実際の応答を表していない
 * ものはここで直す。以前は応答の型が代入先から推論されていたので、ずれていても表に
 * 出なかった (#3439)。
 */
export type ResponseOverrides = {
	/** The signed-in account's public DTO includes its federation state. */
	'i': WithFederationStatus<Misskey.Endpoints['i']['res']>;
	/** Elythia adds the software whose activities the inbox refuses (#3362). */
	'admin/meta': Misskey.entities.AdminMetaResponse & {
		/**
		 * Software whose activities the inbox refuses. Missing on upstream
		 * backends and on older Elythia.
		 */
		inboxSuspendedSoftware?: SuspendedSoftware[];
	};
	/**
	 * Elythia adds the plugin that manages the account (#3468). Missing on
	 * upstream backends.
	 */
	'admin/show-user': Misskey.entities.AdminShowUserResponse & {
		/**
		 * The name of the plugin that manages this account, or `null` for an
		 * ordinary account. Nobody can sign in to a plugin-managed account.
		 */
		managedByPlugin?: string | null;
	};
	/** Elythia adds `isInboxSuspended` to each instance (#3362). */
	'federation/instances': FederationInstance[];
	/**
	 * Elythia adds `isInboxSuspended` (#3362). `null` when the host is unknown,
	 * as upstream.
	 */
	'federation/show-instance': FederationInstance | null;
	/**
	 * The stored value, which is any JSON the client wrote. misskey-js has
	 * `Record<string, never>` (an object with no keys).
	 *
	 * 値の形は書いた側のクライアントしか知らないので、読む側で型を表明する。
	 */
	'i/registry/get': unknown;
	/** Elythia adds `visibility` (#3479). */
	'pages/show': Page;
};
