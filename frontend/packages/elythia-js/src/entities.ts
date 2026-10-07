/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type * as Misskey from 'misskey-js';

/** A field of the signup application form (approval-based signup). */
export type SignupApplicationFormField = {
	label: string;
	type: 'text' | 'textarea';
	required: boolean;
	maxLength?: number;
};

/**
 * Elythia-specific fields that `/api/meta` adds to the upstream response.
 *
 * どれも追加の項目で、古い版のサーバーや、端末に残った古い meta では欠けうる
 * ので、すべて省略可能にしてある。
 */
export type ElythiaMetaFields = {
	/** The running Elythia version (`version` stays the compatible Misskey version). */
	mkGoVersion?: string;
	/** The short commit hash the server was built from, or an empty string. */
	mkGoCommit?: string;
	minimumUsernameLength?: number;
	approvalRequiredForSignup?: boolean;
	registrationClosed?: boolean;
	signupApplicationForm?: SignupApplicationFormField[];
	/** Present only when chunked upload is available. */
	chunkedUpload?: {
		chunkSize: number;
	};
};

/**
 * The state of a signup application (approval-based signup).
 *
 * rejected / expired / completed は終端で、再申請は別の申請になる。
 */
export type SignupApplicationStatus = 'pending' | 'approved' | 'rejected' | 'expired' | 'completed';

/** What the applicant may see about their own application. */
export type SignupApplicationView = {
	status: SignupApplicationStatus;
	createdAt: string;
	expiresAt: string;
};

/**
 * An answer to the application form, stored with the label it was asked under.
 *
 * ラベルを同梱するので、フォームの定義を後から変えても既存の申請が読める (#2570)。
 */
export type SignupApplicationAnswer = {
	label: string;
	value: string;
};

/**
 * A signup application as moderators see it.
 *
 * クレームコードは hash しか持たないので、ここにも出てこない。
 */
export type AdminSignupApplication = {
	id: string;
	status: SignupApplicationStatus;
	answers: SignupApplicationAnswer[];
	createdAt: string;
	updatedAt: string;
	expiresAt: string;
	processedById: string | null;
	processedAt: string | null;
	usedById: string | null;
};

export type EmojiApplicationStatus = 'pending' | 'approved' | 'rejected' | 'canceled';

/** `own` uploads a drive file; `remote` imports an emoji from another server. */
export type EmojiApplicationKind = 'own' | 'remote';

/**
 * Which image an applicant-side preview resolved to, and whether it did.
 *
 * URL が空かどうかで状態を推測させないために、理由を `state` で返す (#2989)。
 */
export type EmojiApplicationPreview = {
	/** Empty unless `state` is `available`. */
	url: string;
	source: 'applicationFile' | 'remoteEmoji' | 'approvedEmoji';
	state: 'available' | 'sourceGone' | 'approvedEmojiGone' | 'unknown';
};

/** Fields shared by the applicant and moderator views of an emoji application. */
type EmojiApplicationBase = {
	id: string;
	kind: EmojiApplicationKind;
	status: EmojiApplicationStatus;
	name: string;
	category: string | null;
	aliases: string[];
	license: string;
	isSensitive: boolean;
	comment: string | null;
	createdAt: string;
	processedAt: string | null;
	rejectReason?: string;
	/** The emoji created on approval. */
	emojiId?: string;
	/** Present only when `kind` is `remote`. */
	remoteHost?: string;
	/** Present only when `kind` is `remote`. */
	remoteName?: string;
};

/**
 * An emoji application as the applicant sees it.
 *
 * 審査したモデレーターは申請者に見せない。
 */
export type EmojiApplication = EmojiApplicationBase & {
	preview: EmojiApplicationPreview;
};

/** An emoji application as moderators see it. */
export type AdminEmojiApplication = EmojiApplicationBase & {
	userId: string;
	/**
	 * The image to review. An empty string means it is gone, and `null` means it
	 * could not be checked.
	 */
	url: string | null;
	fileType: string | null;
	// remoteGone / nameConflict の null を false と同一視しない。確認できていない
	// ことを隠すと、承認を押してから落ちる
	/**
	 * Present only when `kind` is `remote`. `true` means the remote emoji is gone
	 * (approval would fail), and `null` means it could not be checked.
	 */
	remoteGone?: boolean | null;
	/**
	 * Present only while pending. The local emoji that already has this name,
	 * `false` when none does, or `null` when it could not be checked.
	 */
	nameConflict?: { emojiId: string; createdAt: string | null } | false | null;
};

/** Why a past application was judged related to the one under review. */
export type EmojiApplicationMatchedBy = 'name' | 'remoteSource' | 'fileHash';

export type RelatedEmojiApplication = AdminEmojiApplication & {
	/** Every reason that matched. */
	matchedBy: EmojiApplicationMatchedBy[];
};

/** Applications broken down by status. */
export type EmojiApplicationStatusCounts = {
	total: number;
	pending: number;
	approved: number;
	rejected: number;
	canceled: number;
};

/** The usage of one rolling window of the per-user application limit. */
export type EmojiApplicationQuotaWindow = {
	period: string;
	used: number;
	/** Zero or less when the window is unlimited. */
	limit: number;
	unlimited: boolean;
	/** Present only when the window is full. */
	retryAt?: string;
};

/** The usage of the awaiting-review limit. */
export type EmojiApplicationPendingLimit = {
	used: number;
	limit: number;
	unlimited: boolean;
};

/** A manual reset of a user's application limit. */
export type EmojiApplicationQuotaReset = {
	at: string;
	byId: string;
	reason: string;
};

export type MetaDetailed = Misskey.entities.MetaDetailed & ElythiaMetaFields;
