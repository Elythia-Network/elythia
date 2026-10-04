/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { instance } from '@/instance.js';

/**
 * Whether the server accepts no registrations at all (mk-go, #3186).
 *
 * mk-go 独自の meta なので misskey-js の型集合には無い。**招待制
 * (`disableRegistration`) と区別する** — 閉じている間は招待コードも使えないので、
 * 「招待制です」と出すと利用者はコードを探しに行く。
 */
export function isRegistrationClosed(): boolean {
	return (instance as unknown as Record<string, unknown>).registrationClosed === true;
}

/**
 * Whether an API error means registration is closed. The signup routes return
 * it even for requests that were started before closing.
 */
export function isRegistrationClosedError(err: unknown): boolean {
	return (err as { code?: unknown } | null)?.code === 'REGISTRATION_CLOSED';
}

export type RegistrationMode = 'open' | 'invite' | 'approval' | 'closed';

/**
 * Derives the registration mode shown on the moderation page from meta, or
 * `null` when the values match none of the modes.
 *
 * 「受け付けない」は他より優先する。閉じている間も承認制の値は残っている (解除後に
 * 戻れるように。admin の normalizeRegistrationClosed) ので、先に見る。
 *
 * **承認制と招待制が重なった状態はどれにも当てはめない。** 承認制の入口は招待制で
 * 塞がり (`approvalOpen`)、`/api/signup` は承認制で塞がるので、実際にはどこからも
 * 登録できない。「承認制」と出すと動いていると読まれ、同じ選択肢は選び直せないので
 * 直す手段も無くなる。#3186 より前のモデレーター非アクティブ時の自動処理や、API の
 * 直接操作で作られうる。
 */
export function registrationModeOf(state: { closed: boolean; approval: boolean; disableRegistration: boolean }): RegistrationMode | null {
	if (state.closed) return 'closed';
	if (state.approval) return state.disableRegistration ? null : 'approval';
	if (state.disableRegistration) return 'invite';
	return 'open';
}

/**
 * The admin/update-meta fields to send for each registration mode.
 *
 * **3 つとも明示して送る** — 省略するとサーバー側の整合 (#2565 / #2803 / #3186) が
 * 既定を補い、選んだものと違う受け付け方になりうる。「受け付けない」だけは承認制の値を
 * 送らない (閉じている間も残し、申請者の照会を開けておくため)。
 */
export const registrationModePatch: Record<RegistrationMode, Record<string, boolean>> = {
	open: { registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: false },
	invite: { registrationClosed: false, approvalRequiredForSignup: false, disableRegistration: true },
	approval: { registrationClosed: false, approvalRequiredForSignup: true, disableRegistration: false },
	closed: { registrationClosed: true },
};
