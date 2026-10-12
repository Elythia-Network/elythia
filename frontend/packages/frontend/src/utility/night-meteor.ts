/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Elythia: when to show the meteors on the screen at night (#3530).
 * The drawing itself is in `night-meteor-effect.ts`, which is loaded lazily.
 */

/** The hour the night starts, in the device's local time. */
export const NIGHT_START_HOUR = 20;

/** The hour the night ends, in the device's local time. */
export const NIGHT_END_HOUR = 4;

/**
 * The longest wait before checking the conditions again.
 *
 * 次の境目までの時間をそのまま待つと、端末のスリープ中にタイマーが止まったり、
 * 時計やタイムゾーンが変わったりしたときに、境目を大きく過ぎてから気付くことになる。
 * 確かめ直すのは条件の判定だけで軽いので、長くてもこの間隔で見直す。
 */
export const MAX_NIGHT_CHECK_INTERVAL_MS = 15 * 60 * 1000;

/** Whether the hour (0 to 23, local time) is in the night. */
export function isNightHour(hour: number): boolean {
	return hour >= NIGHT_START_HOUR || hour < NIGHT_END_HOUR;
}

/**
 * Returns the milliseconds from `now` to the next start or end of the night,
 * in the device's local time.
 */
export function msUntilNextNightBoundary(now: Date): number {
	const y = now.getFullYear();
	const m = now.getMonth();
	const d = now.getDate();
	// Date のコンストラクタに地方時で組ませると、夏時間の切り替わる日も正しく扱える。
	const todayEnd = new Date(y, m, d, NIGHT_END_HOUR);
	if (todayEnd.getTime() > now.getTime()) return todayEnd.getTime() - now.getTime();
	const todayStart = new Date(y, m, d, NIGHT_START_HOUR);
	if (todayStart.getTime() > now.getTime()) return todayStart.getTime() - now.getTime();
	return new Date(y, m, d + 1, NIGHT_END_HOUR).getTime() - now.getTime();
}

/** Returns how long to wait before checking the conditions again. */
export function nightMeteorCheckDelay(now: Date): number {
	return Math.min(msUntilNextNightBoundary(now), MAX_NIGHT_CHECK_INTERVAL_MS);
}

/** What decides whether the meteors are shown. */
export type NightMeteorConditions = {
	/** The preference `enableNightMeteorEffect`. */
	enabled: boolean;
	/** Whether the applied theme is a dark one (`data-color-scheme` on `<html>`). */
	darkMode: boolean;
	/** The in-app animation preference (`prefer.s.animation`). */
	animation: boolean;
	/** Whether the device asks for less motion (`prefers-reduced-motion: reduce`). */
	reducedMotion: boolean;
	/** Whether the page is visible (`document.visibilityState`). */
	visible: boolean;
	/** Whether the seasonal screen effect (snow or sakura) is on the screen. */
	seasonalEffectShowing: boolean;
	/** The current time. */
	now: Date;
};

/** Whether the meteors should be shown now. */
export function shouldShowNightMeteor(c: NightMeteorConditions): boolean {
	if (!c.enabled) return false;
	if (!c.darkMode) return false;
	// アプリの「アニメーション」をオフにしている人にも、動くものを出さない。
	if (!c.animation) return false;
	if (c.reducedMotion) return false;
	if (!c.visible) return false;
	// 雪や桜と重ねると画面が騒がしくなるので、季節の演出を優先する。
	if (c.seasonalEffectShowing) return false;
	return isNightHour(c.now.getHours());
}
