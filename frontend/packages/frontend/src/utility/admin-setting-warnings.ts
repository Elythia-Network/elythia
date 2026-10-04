/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { ref } from 'vue';
import { misskeyApi } from '@/utility/misskey-api.js';

/**
 * Setting warnings on the control panel top that an admin may hide (mk-go, #3190).
 *
 * **未対応の通報の警告は含めない。** 対応すれば消える、行動を促すための表示なので
 * 閉じさせない。
 */
export const SETTING_WARNING_IDS = ['maintainer', 'inquiryUrl', 'botProtection', 'emailServer'] as const;

export type SettingWarningId = typeof SETTING_WARNING_IDS[number];

// レジストリはアカウントごとなので、端末をまたいで共有され、他の管理者には影響しない。
// preferences は同期が項目ごとの opt-in (既定は無効) なので使わない。
const REGISTRY_SCOPE = ['mkgo', 'adminPanel'];
const REGISTRY_KEY = 'dismissedSettingWarnings';

/**
 * Keeps only known warning ids from a stored value. The registry can be
 * written from anywhere (registry page, other clients), so it is not trusted.
 */
export function parseDismissedWarnings(value: unknown): SettingWarningId[] {
	if (!Array.isArray(value)) return [];
	const ids: SettingWarningId[] = [];
	for (const v of value) {
		if ((SETTING_WARNING_IDS as readonly unknown[]).includes(v) && !ids.includes(v as SettingWarningId)) {
			ids.push(v as SettingWarningId);
		}
	}
	return ids;
}

/**
 * Splits the warnings whose condition currently holds into the ones to show and
 * the ones the admin has hidden.
 *
 * お一人様モードでは 4 つとも出さない (管理者も利用者も自分だけなので、どれも要らない)。
 * 非表示の一覧にも数えない — 再表示しても出ないものを「非表示にした」と見せないため。
 */
export function classifySettingWarnings(opts: {
	active: Record<SettingWarningId, boolean>;
	singleUserMode: boolean;
	dismissed: readonly SettingWarningId[];
}): { visible: SettingWarningId[]; hidden: SettingWarningId[] } {
	if (opts.singleUserMode) return { visible: [], hidden: [] };
	const visible: SettingWarningId[] = [];
	const hidden: SettingWarningId[] = [];
	for (const id of SETTING_WARNING_IDS) {
		if (!opts.active[id]) continue;
		(opts.dismissed.includes(id) ? hidden : visible).push(id);
	}
	return { visible, hidden };
}

/**
 * Whether the bot protection warning applies.
 *
 * 本家は「登録が開いていて captcha がどれも無効」だけを見る。mk-go の承認制
 * (`approvalRequiredForSignup`, #2557) はそれ自体が登録のゲートなので、そのときは出さない。
 */
export function lacksBotProtection(meta: {
	disableRegistration: boolean;
	approvalRequiredForSignup?: boolean;
	enableHcaptcha: boolean;
	enableRecaptcha: boolean;
	enableTurnstile: boolean;
	enableMcaptcha: boolean;
}): boolean {
	if (meta.disableRegistration || meta.approvalRequiredForSignup === true) return false;
	return !meta.enableHcaptcha && !meta.enableRecaptcha && !meta.enableTurnstile && !meta.enableMcaptcha;
}

/**
 * Loads the hidden warnings of the signed-in account. A missing key means none;
 * `null` means they could not be read.
 *
 * **読めないことと、まだ無いことを区別する。** どちらも空にすると、障害の間に 1 つ
 * 閉じただけで保存済みの非表示が消える (上書きするので)。呼び出し側は `null` のとき
 * 閉じるボタンを出さない。
 */
export async function loadDismissedWarnings(): Promise<SettingWarningId[] | null> {
	try {
		return parseDismissedWarnings(await misskeyApi('i/registry/get', { scope: REGISTRY_SCOPE, key: REGISTRY_KEY }));
	} catch (err) {
		if ((err as { code?: unknown } | null)?.code === 'NO_SUCH_KEY') return [];
		return null;
	}
}

let pending: Promise<unknown> = Promise.resolve();

/**
 * Applies `op` to the stored hidden warnings and returns what was saved.
 *
 * **保存のたびに読み直し、1 つずつ順に行う。** 手元の値から書くと、別の端末で閉じた分や、
 * 直前の保存と前後した分を上書きで消す。読めなかったときは書かずに失敗する。
 */
export function updateDismissedWarnings(op: (current: SettingWarningId[]) => SettingWarningId[]): Promise<SettingWarningId[]> {
	const run = pending.then(async () => {
		const current = await loadDismissedWarnings();
		if (current == null) throw new Error('cannot read dismissed setting warnings');
		const next = op(current);
		await misskeyApi('i/registry/set', { scope: REGISTRY_SCOPE, key: REGISTRY_KEY, value: next });
		return next;
	});
	pending = run.catch(() => {});
	return run;
}

/**
 * Single user mode as last known on this page (`null` until loaded). Updated by
 * the general settings page on save, so the control panel top — which stays
 * mounted next to it — reflects the switch without a reload.
 */
export const knownSingleUserMode = ref<boolean | null>(null);
