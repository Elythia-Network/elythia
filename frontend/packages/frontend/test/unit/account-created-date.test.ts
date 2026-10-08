/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import { accountCreatedDateString } from '@/utility/account-created-date.js';

describe('accountCreatedDateString', () => {
	test('日付の単位の値 (00:00Z) は、UTC より西の地域でも前日にしない', () => {
		expect(accountCreatedDateString('2017-04-08T00:00:00.000Z', 'en-US', 'America/Los_Angeles')).toBe('4/8/2017');
	});

	test('日付の単位の値は、UTC より東の地域でも同じ日付', () => {
		expect(accountCreatedDateString('2017-04-08T00:00:00.000Z', 'en-US', 'Asia/Tokyo')).toBe('4/8/2017');
	});

	test('時刻まで分かる値は、地域の日付で出す', () => {
		expect(accountCreatedDateString('2017-04-08T20:00:00.000Z', 'en-US', 'Asia/Tokyo')).toBe('4/9/2017');
		expect(accountCreatedDateString('2017-04-08T00:00:00.001Z', 'en-US', 'America/Los_Angeles')).toBe('4/7/2017');
	});

	test('時刻を出さない', () => {
		const s = accountCreatedDateString('2021-03-04T05:06:07.890Z', 'en-US', 'UTC');
		expect(s).toBe('3/4/2021');
		expect(s).not.toMatch(/:/);
	});

	test('知らない locale でも落ちない', () => {
		expect(accountCreatedDateString('2017-04-08T00:00:00.000Z', 'not a locale!!', 'UTC')).toBe('4/8/2017');
	});
});
