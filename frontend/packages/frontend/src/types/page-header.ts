/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

export type PageHeaderItem = {
	text?: string;
	icon: string;
	highlighted?: boolean;
	danger?: boolean;
	/**
	 * Renders the action greyed out and non-interactive.
	 *
	 * mk-go fork: 上流に存在するが mk-go では意図的に機能を持たない操作を、
	 * 消さずに「あるが使えない」状態で見せるために使う。消すと実装漏れに
	 * 見えるうえ、drop-in で TS へ戻したときの挙動差に気付けなくなる。
	 */
	disabled?: boolean;
	handler: (ev: PointerEvent) => void;
};
