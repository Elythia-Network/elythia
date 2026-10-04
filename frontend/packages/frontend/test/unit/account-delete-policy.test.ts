/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import {
	isAccountDeletionAllowed,
	shouldShowAccountDeletionSection,
} from '@/utility/account-delete-policy.js';

describe('isAccountDeletionAllowed', () => {
	test.each([
		[{ canDeleteAccount: true }, true],
		[{ canDeleteAccount: false }, false],
		[{}, true],
		[{ canDeleteAccount: 'invalid' }, true],
	])('hides the action only for an explicit false value', (policies, expected) => {
		expect(isAccountDeletionAllowed(policies)).toBe(expected);
	});

	test('keeps the deletion-in-progress section visible when the policy is false', () => {
		expect(shouldShowAccountDeletionSection({ canDeleteAccount: false }, true)).toBe(true);
	});

	test('hides a non-deleted account section when the policy is false', () => {
		expect(shouldShowAccountDeletionSection({ canDeleteAccount: false }, false)).toBe(false);
	});
});
