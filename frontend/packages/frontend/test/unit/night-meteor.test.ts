/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import {
	isNightHour,
	MAX_NIGHT_CHECK_INTERVAL_MS,
	msUntilNextNightBoundary,
	nightMeteorCheckDelay,
	shouldShowNightMeteor,
} from '@/utility/night-meteor.js';
import type { NightMeteorConditions } from '@/utility/night-meteor.js';
import { seasonalScreenEffectOf } from '@/utility/seasonal-screen-effect.js';

const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;

/** Builds a local time on 2026-10-13. */
function at(hour: number, minute = 0, second = 0): Date {
	return new Date(2026, 9, 13, hour, minute, second);
}

function conditions(overrides: Partial<NightMeteorConditions> = {}): NightMeteorConditions {
	return {
		enabled: true,
		darkMode: true,
		animation: true,
		reducedMotion: false,
		visible: true,
		seasonalEffectShowing: false,
		now: at(22),
		...overrides,
	};
}

/**
 * Elythia: 夜(20時〜翌4時)でダークテーマのときにだけ、画面に流れ星を流す (#3530)。
 */
describe('isNightHour', () => {
	test.each([
		[19, false], [20, true], [23, true], [0, true], [3, true], [4, false], [12, false],
	])('hour %i -> %s', (hour, expected) => {
		expect(isNightHour(hour)).toBe(expected);
	});
});

describe('shouldShowNightMeteor', () => {
	test.each([
		['19:59', at(19, 59, 59), false],
		['20:00', at(20, 0), true],
		['03:59', at(3, 59, 59), true],
		['04:00', at(4, 0), false],
		['12:00', at(12, 0), false],
	])('at %s -> %s', (_, now, expected) => {
		expect(shouldShowNightMeteor(conditions({ now }))).toBe(expected);
	});

	test('shows on a dark theme at night', () => {
		expect(shouldShowNightMeteor(conditions())).toBe(true);
	});

	test('does not show on a light theme', () => {
		expect(shouldShowNightMeteor(conditions({ darkMode: false }))).toBe(false);
	});

	test('does not show when the preference is off', () => {
		expect(shouldShowNightMeteor(conditions({ enabled: false }))).toBe(false);
	});

	test('does not show when the in-app animation is off', () => {
		expect(shouldShowNightMeteor(conditions({ animation: false }))).toBe(false);
	});

	test('does not show when the device asks for less motion', () => {
		expect(shouldShowNightMeteor(conditions({ reducedMotion: true }))).toBe(false);
	});

	test('does not show while the page is hidden', () => {
		expect(shouldShowNightMeteor(conditions({ visible: false }))).toBe(false);
	});

	test('does not show while the seasonal effect is showing', () => {
		expect(shouldShowNightMeteor(conditions({ seasonalEffectShowing: true }))).toBe(false);
	});

	// main-boot は季節の演出を seasonalScreenEffectOf で決め、出したかどうかを渡す。
	// その組み合わせで、雪や桜の月には流れず、それ以外の月には流れることを確かめる。
	test.each([
		['N', 12, false],
		['N', 1, false],
		['N', 3, false],
		['N', 2, true],
		['N', 7, true],
		['S', 7, false],
		['S', 12, true],
		['S', 3, true],
	] as const)('hemisphere %s, month %i, seasonal effect on -> %s', (hemisphere, month, expected) => {
		const now = new Date(2026, month - 1, 10, 22);
		const seasonalEffectShowing = seasonalScreenEffectOf(now.getMonth() + 1, hemisphere) != null;
		expect(shouldShowNightMeteor(conditions({ now, seasonalEffectShowing }))).toBe(expected);
	});
});

describe('msUntilNextNightBoundary', () => {
	test.each([
		['19:00', at(19, 0), HOUR],
		['19:59:59', at(19, 59, 59), 1000],
		['20:00 (the night has just started; next is 04:00)', at(20, 0), 8 * HOUR],
		['23:30', at(23, 30), 4 * HOUR + 30 * MINUTE],
		['00:00', at(0, 0), 4 * HOUR],
		['03:59', at(3, 59), MINUTE],
		['04:00 (the night has just ended; next is 20:00)', at(4, 0), 16 * HOUR],
		['12:00', at(12, 0), 8 * HOUR],
	])('at %s', (_, now, expected) => {
		expect(msUntilNextNightBoundary(now)).toBe(expected);
	});

	test('lands exactly on a boundary', () => {
		for (let h = 0; h < 24; h++) {
			const now = at(h, 17, 23);
			const next = new Date(now.getTime() + msUntilNextNightBoundary(now));
			expect([20, 4]).toContain(next.getHours());
			expect(next.getMinutes()).toBe(0);
			expect(next.getSeconds()).toBe(0);
			// 境目の時刻ちょうどに、夜かどうかが切り替わる。
			const before = new Date(next.getTime() - 1);
			expect(isNightHour(next.getHours())).not.toBe(isNightHour(before.getHours()));
		}
	});
});

describe('nightMeteorCheckDelay', () => {
	test('waits until the boundary when it is near', () => {
		expect(nightMeteorCheckDelay(at(19, 55))).toBe(5 * MINUTE);
	});

	test('checks again within the cap when the boundary is far', () => {
		expect(nightMeteorCheckDelay(at(12, 0))).toBe(MAX_NIGHT_CHECK_INTERVAL_MS);
	});
});
