/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type * as Misskey from 'misskey-js';
import type { ElythiaEndpoints } from './endpoints.js';

export type { ElythiaEndpoints } from './endpoints.js';
export type {
	AdminEmojiApplication,
	AdminSignupApplication,
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
	FederationRule,
	FederationRuleBody,
	FederationRuleHit,
	FederationRuleMode,
	FederationRuleTarget,
	MetaDetailed,
	RelatedEmojiApplication,
	SignupApplicationAnswer,
	SignupApplicationFormField,
	SignupApplicationStatus,
	SignupApplicationView,
} from './entities.js';

/** misskey-js の Endpoints に、Elythia 独自のエンドポイントを重ねたもの。 */
export type Endpoints = Misskey.Endpoints & ElythiaEndpoints;

/**
 * The response type of an endpoint.
 *
 * 本家のエンドポイントは misskey-js の解決 ($switch による引数ごとの出し分けを
 * 含む) に任せ、独自のエンドポイントは書いた res をそのまま返す。
 */
export type SwitchCaseResponseType<E extends keyof Endpoints, P extends Endpoints[E]['req']> =
	E extends keyof Misskey.Endpoints
		// P をそのまま条件型に入れると、P が和集合のときに分配されて本家と結果が
		// 変わる (users/show の引数を省いた呼び出しなど)。交差で制約を満たして渡す
		? Misskey.api.SwitchCaseResponseType<E, P & Misskey.Endpoints[E]['req']>
		: E extends keyof ElythiaEndpoints ? ElythiaEndpoints[E]['res'] : never;
