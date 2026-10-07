/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type * as Misskey from 'misskey-js';
import type { ElythiaEndpoints } from './endpoints.js';
import type { ElythiaRequestExtensions, ResponseOverrides } from './extensions.js';

export type { ElythiaEndpoints } from './endpoints.js';
export type { ElythiaRequestExtensions, ResponseOverrides } from './extensions.js';
export type {
	AdminEmojiApplication,
	AdminSignupApplication,
	BubbleGameMode,
	BubbleVersusMatch,
	BubbleVersusReason,
	BubbleVersusRecord,
	BubbleVersusRecordResult,
	BubbleVersusReportReason,
	BubbleVersusResultSummary,
	BubbleVersusStatus,
	DatabaseHealthProblem,
	DatabaseHealthReport,
	DatabaseIndexStat,
	DatabaseTableStat,
	DeliveryBreakerState,
	DeliveryOutcomeClass,
	DriveUsage,
	DriveUsageBucket,
	DriveUsageKind,
	DriveUsageOrigin,
	ElythiaMetaFields,
	EmojiApplication,
	EmojiApplicationKind,
	EmojiApplicationMatchedBy,
	EmojiApplicationPendingLimit,
	EmojiApplicationPreview,
	EmojiApplicationQuotaReset,
	EmojiApplicationQuotaWindow,
	EmojiApplicationStatus,
	EmojiApplicationStatusCounts,
	FederationHealthReport,
	FederationHostHealth,
	FederationRule,
	FederationRuleBody,
	FederationRuleHit,
	FederationRuleMode,
	FederationRuleTarget,
	GoneInstance,
	GoneInstanceCleanResult,
	InboxOutcomeClass,
	IPAccountsResult,
	IPLookupKind,
	IPLookupLogEntry,
	IPLookupLogResult,
	IPRelatedAccountsResult,
	IPRelatedCandidate,
	IPRelatedSharedIP,
	IPSearchAccount,
	JoinedChatRoom,
	LegacySigninResponse,
	MetaDetailed,
	RelatedEmojiApplication,
	RemoteCheckReport,
	RemoteEmojiMeta,
	RoleAssignmentLookup,
	SelfCheckReport,
	SelfCheckResult,
	SelfCheckStatus,
	ServerMetrics,
	ServerPluginInfo,
	SignupApplicationAnswer,
	SignupApplicationFormField,
	SignupApplicationStatus,
	SignupApplicationView,
} from './entities.js';

/**
 * Accepts only maps whose keys are upstream endpoints.
 *
 * 本家に無いキーを書くと、Endpoints に req か res の欠けたエンドポイントが生えてしまう。
 */
type UpstreamEndpointMap<T extends { [E in keyof T]: E extends keyof Misskey.Endpoints ? unknown : never }> = T;

/**
 * misskey-js の Endpoints に、Elythia 独自のエンドポイントと、本家のエンドポイントへ
 * Elythia が足した引数を重ね、型の誤っている応答を差し替えたもの。
 */
export type Endpoints = Omit<Misskey.Endpoints, keyof UpstreamEndpointMap<ResponseOverrides>> & {
	[E in keyof ResponseOverrides]: { req: Misskey.Endpoints[E]['req']; res: ResponseOverrides[E] };
} & ElythiaEndpoints & {
	[E in keyof UpstreamEndpointMap<ElythiaRequestExtensions>]: { req: ElythiaRequestExtensions[E] };
};

/** The keys of every member of a union (`keyof` of a union keeps only the common keys). */
type KeysOfUnion<T> = T extends unknown ? keyof T : never;

/**
 * Maps every key of `P` that the request type of `E` does not have to `never`.
 *
 * 引数の型 P を渡した値から推論すると、P は req の部分型でありさえすればよいので、
 * 余計なキー (省略できる引数の打ち間違いなど) が型エラーにならない (#3439)。推論した P に
 * これを交差させて、req に無いキーを弾く。req が和集合のときは、どれかの要素にあるキーを
 * 許す (keyof を和集合に取ると共通のキーしか残らない)。`EmptyRequest` のように
 * `Record<string, unknown>` を含む req は、どのキーも許す。
 */
export type ExcessKeys<E extends keyof Endpoints, P, Allowed extends PropertyKey = never> = {
	[K in Exclude<KeysOfUnion<P>, KeysOfUnion<Endpoints[E]['req']> | Allowed>]: never;
};

/**
 * The response type of an endpoint.
 *
 * 本家のエンドポイントは misskey-js の解決 ($switch による引数ごとの出し分けを
 * 含む) に任せ、独自のエンドポイントは書いた res をそのまま返す。
 */
export type SwitchCaseResponseType<E extends keyof Endpoints, P extends Endpoints[E]['req']> =
	E extends keyof ResponseOverrides ? ResponseOverrides[E]
	: E extends keyof Misskey.Endpoints
		// P をそのまま条件型に入れると、P が和集合のときに分配されて本家と結果が
		// 変わる (users/show の引数を省いた呼び出しなど)。交差で制約を満たして渡す
		? Misskey.api.SwitchCaseResponseType<E, P & Misskey.Endpoints[E]['req']>
		: E extends keyof ElythiaEndpoints ? ElythiaEndpoints[E]['res'] : never;
