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

/**
 * A chat room as `chat/rooms/joined` returns it.
 *
 * 一覧の取得で owner を読み込んでいないので、owner (UserLite) は載らない。
 * isMuted / invitationExists は常に入れて返す。
 */
export type JoinedChatRoom = Omit<Misskey.entities.ChatRoom, 'owner' | 'isMuted' | 'invitationExists'> & {
	isMuted: boolean;
	invitationExists: boolean;
};

/**
 * The response of the legacy `signin` endpoint.
 *
 * signin-flow と違い、captcha の段は無く、passkey の段でも authRequest を返さない。
 * 2FA を完了する手段が無いので、2FA が有効な利用者は signin-flow でやり直す。
 */
export type LegacySigninResponse = {
	finished: true;
	id: string;
	/** The access token. */
	i: string;
} | {
	finished: false;
	/**
	 * `password` when no password was sent. `totp` or `passkey` when the
	 * password was correct but two-factor authentication is enabled.
	 */
	next: 'password' | 'totp' | 'passkey';
};

/**
 * Whether the caller (or, on the admin side, a user) has an exact assignment
 * of a role.
 *
 * role_assignment の行だけを見るので、conditional な role は条件を満たして
 * いても、行が無ければ assigned が false になる。判別できるように role.target を返す (#2633)。
 */
export type RoleAssignmentLookup = {
	/**
	 * Whether an active (unexpired) assignment row exists. The conditions of a
	 * conditional role are not evaluated, but a conditional role can still be
	 * `true` when a row exists (assigned manually, or left from before the role
	 * was switched to conditional).
	 */
	assigned: boolean;
	/** `null` when not assigned, or when the assignment never expires. */
	expiresAt: string | null;
	role: {
		id: string;
		target: 'manual' | 'conditional';
		isPublic: boolean;
		canEditMembersByModerator: boolean;
	};
};

export type MetaDetailed = Misskey.entities.MetaDetailed & ElythiaMetaFields;

/**
 * Elythia-specific fields that the detailed user (`users/show` and others) adds
 * to the upstream response.
 *
 * 古い版のサーバーでは欠けるので省略可能にしてある。
 */
export type ElythiaUserDetailedFields = {
	/**
	 * When a remote user created the account (ISO 8601), read from the actor's
	 * `published` or the origin server's `users/show`. Absent for local users
	 * and when unknown. `createdAt` of a remote user is when this server first
	 * saw the account.
	 */
	accountCreatedAt?: string | null;
};

export type UserDetailed = Misskey.entities.UserDetailed & ElythiaUserDetailedFields;

/** The outcome of one check of `admin/self-check` or `admin/federation/check-host`. */
export type SelfCheckStatus = 'ok' | 'warn' | 'fail' | 'skip';

/** One check of `admin/self-check` or `admin/federation/check-host`. */
export type SelfCheckResult = {
	/** The check's name. The server may add checks, so this is not a closed set. */
	name: string;
	status: SelfCheckStatus;
	detail: string;
	/** How to fix it. Always present when `status` is `fail` or `warn`. */
	hint?: string;
};

/** The result of `admin/self-check`. */
export type SelfCheckReport = {
	results: SelfCheckResult[];
	/** `false` when any check failed. A warning does not make it `false`. */
	ok: boolean;
};

/** The result of `admin/federation/check-host`. */
export type RemoteCheckReport = SelfCheckReport & {
	/** The normalized host that was checked. */
	host: string;
};

/** A goneSuspended instance and the follow relations left with it. */
export type GoneInstance = {
	host: string;
	/** When it was judged gone. `null` when that was not recorded (for example, suspended by Misskey). */
	suspendedAt: string | null;
	followers: number;
	following: number;
	followRequests: number;
};

/** What `admin/federation/clean-gone-instance` removed. */
export type GoneInstanceCleanResult = {
	host: string;
	/** Follows from the gone instance to local users. */
	removedFollowers: number;
	/** Follows from local users to the gone instance. */
	removedFollowing: number;
	removedFollowRequests: number;
	/** Follow relations left over the per-run limit. */
	remaining: number;
};

/** How an outgoing delivery ended. */
export type DeliveryOutcomeClass = 'success' | 'gone' | 'rateLimited' | 'clientError' | 'serverError' | 'transport';

/** How an incoming activity was handled. */
export type InboxOutcomeClass =
	| 'accepted'
	| 'unsupported'
	| 'signatureFailed'
	| 'blocked'
	| 'actorUnauthorized'
	| 'ldSignatureFailed'
	| 'processingError'
	| 'duplicate';

/** The outcomes of one host in the window of `admin/federation/delivery-health` or `inbox-health`. */
export type FederationHostHealth<C extends DeliveryOutcomeClass | InboxOutcomeClass = DeliveryOutcomeClass | InboxOutcomeClass> = {
	host: string;
	success: number;
	failure: number;
	/** Only the classes that occurred. */
	byClass: Partial<Record<C, number>>;
	/** The upper bound of the histogram bucket, or -1 past the largest bucket. */
	latencyP50Ms: number;
	/** The upper bound of the histogram bucket, or -1 past the largest bucket. */
	latencyP95Ms: number;
	/** The last failure recorded for the host, if any. */
	lastError?: {
		at: string;
		class: C;
		status: number;
		message: string;
	};
};

/** A host whose deliveries are stopped by the breaker or spaced out after 429. */
export type DeliveryBreakerState = {
	host: string;
	/**
	 * Whether the breaker is open (deliveries are stopped). When `false`, the
	 * host is only throttled after 429, or the delayed deliveries are being sent.
	 */
	open: boolean;
	consecutiveFailures: number;
	openedAt: string | null;
	nextProbeAt: string | null;
	probeIntervalSeconds: number;
	/** Until when deliveries are held after 429. `null` when not throttled. */
	throttledUntil: string | null;
	/** The last time a delayed delivery is scheduled for. `null` when none is left. */
	reservedUntil: string | null;
};

/** The result of `admin/federation/delivery-health` and `inbox-health`. */
export type FederationHealthReport<C extends DeliveryOutcomeClass | InboxOutcomeClass> = {
	/**
	 * The requested window after the default and the cap, in seconds. The counts
	 * are aggregated in whole minutes (at least one), so a shorter or uneven
	 * window covers the minutes it rounds down to.
	 */
	windowSeconds: number;
	/** The most failures first. */
	hosts: FederationHostHealth<C>[];
	/** The total number of hosts dropped by the in-memory cap. */
	evictedHosts: number;
	/** Always empty for `inbox-health`. */
	breakers: DeliveryBreakerState[];
};

/** One local account that used the IP searched with `admin/ip/accounts`. */
export type IPSearchAccount = {
	user: Misskey.entities.UserLite;
	isSuspended: boolean;
	isDeleted: boolean;
	/** The account's last activity (not of this IP). `null` when unknown. */
	lastActiveDate: string | null;
	/** The first observation of this IP by the account. */
	firstSeenAt: string;
	/** The last observation of this IP by the account. */
	lastSeenAt: string;
	/** The number of stored observations, not of connections. */
	observationCount: number;
};

/**
 * The result of `admin/ip/accounts`.
 *
 * 一致が無いことを「記録が無い」と取り違えないための材料 (loggingEnabled /
 * hasAnyHistory / droppedCount) も一緒に返る。
 */
export type IPAccountsResult = {
	/** The normalized form actually searched. */
	ip: string;
	loggingEnabled: boolean;
	hasAnyHistory: boolean;
	sinceDays: number;
	/** How long IP records are kept, in days. */
	retentionDays: number;
	limit: number;
	offset: number;
	/**
	 * Decided before the accounts are resolved. `accounts` can be empty while
	 * this is `true`; then read on with `offset + limit`.
	 */
	hasMore: boolean;
	/** Rows on this page dropped because the account could not be resolved. */
	droppedCount: number;
	accounts: IPSearchAccount[];
};

/** What an IP lookup recorded in `admin/ip/lookup-log` started from. */
export type IPLookupKind = 'ip' | 'relatedAccounts' | 'signins' | 'userIps';

/** One recorded IP lookup. */
export type IPLookupLogEntry = {
	id: string;
	/** The moderator who looked up. `null` when the account cannot be resolved. */
	user: Misskey.entities.UserLite | null;
	userId: string;
	kind: IPLookupKind;
	/** The IP looked up. An empty string unless the lookup started from an IP. */
	ip: string;
	/** The user looked up. `null` when there is none or it cannot be resolved. */
	targetUser: Misskey.entities.UserLite | null;
	/** An empty string when the lookup did not start from a user. */
	targetUserId: string;
	sinceDays: number;
	/** The number of results returned (the results themselves are not recorded). */
	resultCount: number;
	createdAt: string;
};

/** The result of `admin/ip/lookup-log`. */
export type IPLookupLogResult = {
	/** How long lookups are recorded, in days. */
	retentionDays: number;
	limit: number;
	offset: number;
	hasMore: boolean;
	/** Newest first. */
	entries: IPLookupLogEntry[];
};

/** One IP shared by the target and a candidate of `admin/ip/related-accounts`. */
export type IPRelatedSharedIP = {
	ip: string;
	targetLastSeenAt: string;
	candidateLastSeenAt: string;
	/** Accounts that used the IP in the window, including the target. */
	ipAccountCount: number;
	/** When `true`, `ipAccountCount` is a lower bound (the count reached the cap). */
	ipAccountCountIsLowerBound: boolean;
	/** The elapsed days used for the time decay. */
	elapsedDays: number;
};

/** An account that shared at least one IP with the target. */
export type IPRelatedCandidate = {
	user: Misskey.entities.UserLite;
	isSuspended: boolean;
	isDeleted: boolean;
	/** The account's last activity. `null` when unknown. */
	lastActiveDate: string | null;
	sharedIpCount: number;
	/** Only for ordering. Not a probability, and can exceed 1. */
	score: number;
	/** The heaviest first. */
	sharedIps: IPRelatedSharedIP[];
};

/** The result of `admin/ip/related-accounts`. */
export type IPRelatedAccountsResult = {
	/** The target user. */
	user: Misskey.entities.UserLite;
	loggingEnabled: boolean;
	hasAnyHistory: boolean;
	sinceDays: number;
	retentionDays: number;
	/** The half-life of the time decay, in days. */
	halfLifeDays: number;
	/** The number of the target's IPs used (after the cap). */
	targetIpCount: number;
	/** `targetIpsTruncated || candidatesTruncated`. */
	truncated: boolean;
	targetIpsTruncated: boolean;
	candidatesTruncated: boolean;
	limit: number;
	offset: number;
	hasMore: boolean;
	/** Candidates on this page dropped because the account could not be resolved. */
	droppedCount: number;
	candidates: IPRelatedCandidate[];
};

/** An index that has not been scanned since the statistics were reset. */
export type DatabaseIndexStat = {
	table: string;
	index: string;
	scans: number;
	/** An estimate from `pg_class.relpages`. */
	sizeBytes: number;
	/** A unique or primary key index (it backs a constraint). */
	unique: boolean;
	primary: boolean;
};

export type DatabaseTableStat = {
	table: string;
	liveRows: number;
	deadRows: number;
	deadRatio: number;
	/**
	 * An estimate from `pg_class.relpages`, including TOAST and indexes. `null`
	 * when the table has not been measured yet (never vacuumed or analyzed, or
	 * truncated and not measured since).
	 */
	sizeBytes: number | null;
	/** The newer of the manual and the automatic run. `null` when never. */
	lastVacuum: string | null;
	/** The newer of the manual and the automatic run. `null` when never. */
	lastAnalyze: string | null;
	modifiedSinceAnalyze: number;
	/** Known only when the database role can see other roles' VACUUM. */
	vacuuming: boolean;
};

/** A table that looks bloated or not vacuumed enough. */
export type DatabaseHealthProblem = {
	table: string;
	kind: 'bloat' | 'vacuum';
	detail: string;
};

/** The result of `admin/database-health`. */
export type DatabaseHealthReport = {
	generatedAt: string;
	/** The last recorded statistics reset. `null` when none is recorded. */
	statsReset: string | null;
	replicasConfigured: boolean;
	autovacuumThreshold: number;
	autovacuumScaleFactor: number;
	/** Zero or less when the server has no such setting or it is disabled. */
	autovacuumMaxThreshold: number;
	unusedIndexes: DatabaseIndexStat[];
	tables: DatabaseTableStat[];
	problems: DatabaseHealthProblem[];
};

/** The result of `admin/server-metrics`: the running server process. */
export type ServerMetrics = {
	/** Zero when the start time is unknown. */
	uptimeMs: number;
	version: {
		misskey: string;
		mkGo: string;
	};
	go: {
		goroutines: number;
		gomaxprocs: number;
		heapAllocBytes: number;
		heapSysBytes: number;
		heapObjects: number;
		gcNum: number;
		/** Zero before the first GC. */
		lastGcPauseNs: number;
		gcCpuFraction: number;
	};
};

/** A server plugin compiled into the server. */
export type ServerPluginInfo = {
	name: string;
	version: string;
	apiVersion: number;
	/** The runtime setting (`plugins.<name>.enabled`). */
	enabled: boolean;
	/** What the plugin declares, not what this process wired. */
	routes: boolean;
	jobs: boolean;
	effectivePolicies: boolean;
	migrations: number;
	schema: string;
	/** The setting keys only. The values are never returned. */
	configKeys: string[];
	/**
	 * The secret names the plugin declares. Neither the values nor whether they
	 * are set; administrators read that from `plugin/<name>/_secrets`.
	 */
	secrets: string[];
};

/** One secret of a server plugin, as `plugin/<name>/_secrets` returns it. The value is never returned. */
export type PluginSecretInfo = {
	name: string;
	description: string;
	/** Whether the plugin declares it. Only declared secrets can be entered from the control panel. */
	declared: boolean;
	configured: boolean;
	/** False when the stored value does not decrypt with the current `pluginSecretKey`. */
	readable: boolean;
	/** The last 4 characters of a value at least 20 characters long. */
	hint: string | null;
	updatedAt: string | null;
};

/** The result of `plugin/<name>/_secrets` (administrators only). */
export type PluginSecretList = {
	/** False when `pluginSecretKey` is not configured; nothing can be stored or deleted then. */
	available: boolean;
	secrets: PluginSecretInfo[];
};

/** One cell of `admin/drive/usage`. */
export type DriveUsageBucket = {
	/** The number of drive file rows. */
	count: number;
	/** The sum of the rows' `size`, in bytes, as the database knows it. */
	size: number;
	/** How many of `count` are links (rows that hold no bytes of their own). */
	linkCount: number;
};

/** What a drive file is used as, in `admin/drive/usage`. */
export type DriveUsageKind = 'attachment' | 'avatar' | 'banner' | 'emoji' | 'other';

export type DriveUsageOrigin = 'local' | 'remote';

/** The result of `admin/drive/usage`. */
export type DriveUsage = {
	/** When the numbers were aggregated (not when they were returned). */
	calculatedAt: string;
	elapsedMs: number;
	/** Whether a recent snapshot was reused. */
	cached: boolean;
	cacheTtlSeconds: number;
	/** The cap of `byHost` and `byUser`. */
	topLimit: number;
	/** Where the numbers come from. Not the actual usage of the object storage. */
	source: 'database';
	total: DriveUsageBucket;
	local: DriveUsageBucket;
	remote: DriveUsageBucket;
	/** Every kind and origin, zero-filled, in a fixed order. */
	byKind: (DriveUsageBucket & { kind: DriveUsageKind; origin: DriveUsageOrigin })[];
	/** Remote hosts, the largest first. */
	byHost: (DriveUsageBucket & { host: string })[];
	/** Local users, the largest first. `username` is empty when the user cannot be joined. */
	byUser: (DriveUsageBucket & { userId: string; username: string })[];
};

/** The stored remote emoji that `admin/emoji/fetch-remote-meta` looked up. */
type RemoteEmojiMetaSource = {
	emojiId: string;
	name: string;
	host: string;
	originalUrl: string;
};

/**
 * The result of `admin/emoji/fetch-remote-meta`.
 *
 * 取れなかった項目はキーごと出ない (空文字を返すと、既存の値を消す指示に読まれる)。
 * license は相手から取れなかったとき、保存済みの値があればそれが入る。
 */
export type RemoteEmojiMeta =
	| (RemoteEmojiMetaSource & {
		fetched: true;
		category?: string;
		aliases?: string[];
		license?: string;
		isSensitive?: boolean;
	})
	| (RemoteEmojiMetaSource & {
		fetched: false;
		reason: 'unsupported' | 'notFound' | 'error';
		license?: string;
	});

