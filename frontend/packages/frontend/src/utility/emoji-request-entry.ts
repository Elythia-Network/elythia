/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { $i } from '@/i.js';
import type { Ref } from 'vue';
import { computed } from 'vue';

/**
 * カスタム絵文字の登録申請ページ (`/emoji-request`) への導線を出してよいか
 * (#2934 / #2989)。
 *
 * **判定を 1 箇所に集める。** 導線は 3 つある (設定 → その他 / カスタム絵文字
 * 一覧 / インスタンスメニュー) ので、それぞれで同じ式を書くと、条件を変えた
 * ときに一部の画面だけ違う状態が残る。#2984 が「画面が 2 つあるのに片方だけ」で
 * 踏んだのと同じ形。
 *
 * 条件:
 *
 * - ログイン済み
 * - `canRequestCustomEmojis` が true
 * - モデレーターではない
 * - `canManageCustomEmojis` を持っていない
 *
 * **管理権限を持つ人には出さない。** その人は申請ではなく直接登録できるので、
 * 申請ページへ送っても遠回りになるだけ。両方を持つ状態でも管理導線を優先する。
 *
 * **これは UX の制御でしかない。** 申請 API 側のロールポリシー検証
 * (`RequireRolePolicy(canRequestCustomEmojis)`) は別に効いている。
 */
export function canShowEmojiRequestEntry(): boolean {
	if ($i == null) return false;
	const policies = $i.policies as unknown as Record<string, unknown>;
	if ($i.isModerator === true) return false;
	if (policies.canManageCustomEmojis === true) return false;
	return policies.canRequestCustomEmojis === true;
}

/** `canShowEmojiRequestEntry` の computed 版 (テンプレートから使う)。 */
export function useEmojiRequestEntry(): Ref<boolean> {
	return computed(() => canShowEmojiRequestEntry());
}
