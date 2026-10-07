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

/**
 * How a federation rule acts: `disabled` is not evaluated, `record` only
 * records matches, and `enforce` also applies the actions.
 */
export type FederationRuleMode = 'disabled' | 'record' | 'enforce';

/** Whether a federation rule looks at incoming notes or at activities. */
export type FederationRuleTarget = 'note' | 'activity';

/**
 * The editable part of a federation rule.
 *
 * `null` の条件は「問わない」。activity のルールは投稿の中身の条件と書き換えを
 * 持てない (サーバーが弾く)。
 */
export type FederationRuleBody = {
	name: string;
	mode: FederationRuleMode;
	target: FederationRuleTarget;
	/** The evaluation order, from 0. */
	position: number;
	hosts: string[];
	activityTypes: string[];
	isBot: boolean | null;
	newWithinHours: number | null;
	patterns: string[];
	hasAttachment: boolean | null;
	tags: string[];
	reject: boolean;
	stripMedia: boolean;
	sensitive: boolean;
	unlist: boolean;
	cw: string | null;
};

export type FederationRule = FederationRuleBody & {
	id: string;
	createdAt: string;
	updatedAt: string;
	/** Matches in the last 24 hours. Always 0 in the responses of create and update. */
	hits: number;
};

/** One note or activity that matched a federation rule. */
export type FederationRuleHit = {
	ruleId: string;
	host: string;
	subject: string;
	/**
	 * The activity type as received, or `Note` / `Update` for a note (a new note
	 * or an edit).
	 */
	kind: string;
	/** Whether the actions were applied (the rule was enforced). */
	applied: boolean;
	at: string;
};

type BubbleGameModeBase = 'normal' | 'yen' | 'square' | 'sweets' | 'space';

/**
 * A bubble game mode: a shape set, optionally joined with a physics.
 *
 * 型としては misskey-bubble-game の GameMode と同じ集合にしてある (相互に代入
 * できるように)。normal × bouncy は歴史的な名前の `bouncy` で表すので、
 * `normal-bouncy` は型では通るが、サーバーの ValidGameMode とクライアントの
 * parseGameMode が実行時に弾く。
 */
export type BubbleGameMode =
	| BubbleGameModeBase
	| 'bouncy'
	| `${Exclude<BubbleGameModeBase, 'space'>}-${'bouncy' | 'friction'}`;

export type BubbleVersusStatus = 'invited' | 'accepted' | 'playing' | 'ended';

/** How a versus match ended. */
export type BubbleVersusReason = 'gameOver' | 'surrender' | 'timeUp' | 'disconnected' | 'invalidReport';

/**
 * The reason of one player's report. `opponentEnded` is sent after the
 * opponent's report ended the match, only to keep the board for the replay.
 * It never decides the outcome.
 */
export type BubbleVersusReportReason = 'gameOver' | 'surrender' | 'timeUp' | 'opponentEnded';

/** One player's reported result of a running or ended match. */
export type BubbleVersusResultSummary = {
	score: number;
	frame: number;
	reason: BubbleVersusReason | 'opponentEnded';
};

/**
 * A versus match, as its participants see it.
 *
 * 記録 (logs) は大きいので載せない。リプレイは record から引く。
 */
export type BubbleVersusMatch = {
	id: string;
	gameMode: BubbleGameMode;
	status: BubbleVersusStatus;
	/** Decided only when the match is accepted. */
	seed: string | null;
	createdAt: string;
	startAt: string | null;
	endedAt: string | null;
	winnerId: string | null;
	reason: BubbleVersusReason | null;
	user1Id: string;
	user2Id: string;
	/** `null` when the user no longer exists. */
	user1: Misskey.entities.UserLite | null;
	/** `null` when the user no longer exists. */
	user2: Misskey.entities.UserLite | null;
	user1Ready: boolean;
	user2Ready: boolean;
	user1Result: BubbleVersusResultSummary | null;
	user2Result: BubbleVersusResultSummary | null;
};

/** One player's side of a stored match record. */
export type BubbleVersusRecordResult = {
	score: number;
	frame: number;
	reason: BubbleVersusReason | 'opponentEnded';
	/** The engine version the logs were made with. `null` when the client did not send it. */
	gameVersion: number | null;
};

/**
 * A finished match as returned by `bubble-game/versus/history` and `record`.
 * `seed` and the logs are only present on `record`.
 */
export type BubbleVersusRecord = {
	id: string;
	gameMode: BubbleGameMode;
	startedAt: string;
	endedAt: string | null;
	winnerId: string | null;
	reason: BubbleVersusReason | null;
	/** Whether both players made the record public. */
	isPublic: boolean;
	user1Id: string;
	user2Id: string;
	/** `null` when the user no longer exists. */
	user1: Misskey.entities.UserLite | null;
	/** `null` when the user no longer exists. */
	user2: Misskey.entities.UserLite | null;
	user1Public: boolean;
	user2Public: boolean;
	user1Result: BubbleVersusRecordResult | null;
	user2Result: BubbleVersusRecordResult | null;
	seed?: string;
	user1Logs?: number[][] | null;
	user2Logs?: number[][] | null;
};

export type MetaDetailed = Misskey.entities.MetaDetailed & ElythiaMetaFields;
