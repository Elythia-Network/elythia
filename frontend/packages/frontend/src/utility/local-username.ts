/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// mk-go: 新規登録の username の最小文字数 (#3015)。
//
// **純正 backend の meta には無いフィールド**なので、取れないときは upstream と
// 同じ 1 に倒す (mk-go 以外を相手にしてもこの画面が壊れないようにする)。
// 範囲外の値はサーバー側と同じ 1-20 に丸める — ここだけが別の判定をすると
// 「入力できるのに登録で弾かれる」または「弾かれるはずの名前が通ったように
// 見える」形になる (サーバー側は `internal/core/signup` の
// `minimumUsernameLength`)。

/** Lower bound of `localUsernameSchema` (`^\w{1,20}$`). */
export const MIN_USERNAME_LENGTH = 1;

/** Upper bound of `localUsernameSchema` (`^\w{1,20}$`). */
export const MAX_USERNAME_LENGTH = 20;

/**
 * Resolves the minimum username length the instance actually enforces.
 *
 * Returns MIN_USERNAME_LENGTH when the field is absent (upstream Misskey) or
 * not a finite number, and clamps out-of-range values into
 * [MIN_USERNAME_LENGTH, MAX_USERNAME_LENGTH] the same way the server does.
 */
export function resolveMinimumUsernameLength(meta: unknown): number {
	const raw = (meta as { minimumUsernameLength?: unknown } | null | undefined)?.minimumUsernameLength;
	if (typeof raw !== 'number' || !Number.isFinite(raw)) return MIN_USERNAME_LENGTH;
	return Math.min(MAX_USERNAME_LENGTH, Math.max(MIN_USERNAME_LENGTH, Math.floor(raw)));
}

/** Why a username cannot be used, or null when it passes the local checks. */
export type LocalUsernameState = 'invalid-format' | 'min-range' | 'max-range' | null;

/**
 * Classifies a username against the checks the client can do without asking
 * the server: the character set, the configured minimum length, and the
 * `localUsernameSchema` maximum.
 *
 * `minimumLength` is expected to come from resolveMinimumUsernameLength.
 */
export function resolveLocalUsernameState(username: string, minimumLength: number): LocalUsernameState {
	// 文字種を先に見る。**順番は変えない** — 記号入りの短い名前に
	// 「n 文字以上にしてください」を出すと、長くしても通らない案内になる。
	if (!/^[a-zA-Z0-9_]+$/.test(username)) return 'invalid-format';
	if (username.length < minimumLength) return 'min-range';
	if (username.length > MAX_USERNAME_LENGTH) return 'max-range';
	return null;
}
