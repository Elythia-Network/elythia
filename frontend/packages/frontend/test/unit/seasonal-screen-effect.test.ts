/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import { seasonalScreenEffectOf } from '@/utility/seasonal-screen-effect.js';

/**
 * Elythia: 季節の演出を出す月の判定を起動処理から切り出した (#3530)。
 * 夜空の流れ星は、季節の演出が出ている間は出さないので、どの月に出るかを固定しておく。
 */
describe('seasonalScreenEffectOf', () => {
	test.each([
		[1, 'snow'], [2, null], [3, 'sakura'], [4, 'sakura'], [5, null], [6, null],
		[7, null], [8, null], [9, null], [10, null], [11, null], [12, 'snow'],
	] as const)('northern hemisphere, month %i -> %s', (month, expected) => {
		expect(seasonalScreenEffectOf(month, 'N')).toBe(expected);
	});

	test.each([
		[1, null], [2, null], [3, null], [4, null], [5, null], [6, null],
		[7, 'snow'], [8, 'snow'], [9, null], [10, null], [11, null], [12, null],
	] as const)('southern hemisphere, month %i -> %s', (month, expected) => {
		expect(seasonalScreenEffectOf(month, 'S')).toBe(expected);
	});
});
