/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type * as Misskey from 'misskey-js';
import type { SignupApplicationFormField } from './entities.js';

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
		/** At least 1. */
		chunkedUploadMaxSessionsPerUser?: number;
		/** At least 1. */
		chunkedUploadMaxPendingMbPerUser?: number;
		/** Periodically deletes remote users that only relays brought in. */
		enableRelayOrphanUserCleanup?: boolean;
		relayOrphanUserGraceDays?: number;
		/** Keeps notes seen only through relays in Redis instead of the database. */
		enableEphemeralRelayNotes?: boolean;
		ephemeralRelayNoteTtlMinutes?: number;
	};
	'i/regenerate-token': {
		/** The two-factor authentication code. Required when two-factor authentication is enabled. */
		token?: string | null;
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
	/**
	 * The stored value, which is any JSON the client wrote. misskey-js has
	 * `Record<string, never>` (an object with no keys).
	 *
	 * 値の形は書いた側のクライアントしか知らないので、読む側で型を表明する。
	 */
	'i/registry/get': unknown;
};
