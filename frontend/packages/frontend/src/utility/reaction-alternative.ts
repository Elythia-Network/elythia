/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { getEmojiNameFromReaction, isLocalCustomEmojiReaction } from '@@/js/emoji-name.js';
import { bareEmojiName, hasLocalEmojiWithSameName } from '@/utility/import-remote-emoji.js';

/**
 * Returns the local reaction to send instead of a remote one, or `null` when
 * there is no local emoji with the same shortcode.
 *
 * mk-go 独自 (#2697)。リモートのリアクション (`:foo@host:`) は押せないが、
 * **ローカルに同じショートコードの絵文字があるなら、それでリアクションできる**
 * ようにする (俗にいうリアクションの相乗り)。CherryPick 系に同等の実装がある。
 *
 * **返すのは `:foo@.:` (ローカルホストマーク付き)。** `:foo:` でも backend は
 * ローカルの絵文字に解決するが、返ってくる `myReaction` は `:foo@.:` に正規化
 * されるので、送る文字列を揃えておかないと `oldReaction !== sendingReaction` の
 * 比較が外れる。押し直しが「取り消し」ではなく「付け替え」として扱われ、
 * 確認ダイアログの文言まで変わる。
 *
 * **数は合算されない。** リアクションは文字列キーで持つので、`:foo@host:` の
 * チップを押すと `:foo@.:` のチップが別に増える (既にローカルの同名チップが
 * あればそちらが増える)。合算は backend 無改造でもできる (`resolveReactionValue`
 * は任意 host を受ける) が、**純正 TS は任意 host を受けず ❤ に落とす**ので
 * drop-in で戻したときに壊れる。詳細は `docs/divergence.md` §4-2。
 */
export function localAlternativeReaction(reaction: string): string | null {
	// Unicode 絵文字はそのまま押せる。
	if (reaction[0] !== ':') return null;
	// 既にローカルのカスタム絵文字なら相乗りするものが無い。
	if (isLocalCustomEmojiReaction(reaction)) return null;

	const name = getEmojiNameFromReaction(reaction);
	if (!hasLocalEmojiWithSameName(name)) return null;
	return `:${bareEmojiName(name)}@.:`;
}
