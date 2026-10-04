/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import {
	accountDeletionPolicyDefaults,
	accountDeletionPolicyKeys,
	ensureAccountDeletionRolePolicies,
} from '@/utility/account-delete-policy.js';

describe('account deletion role policy behavior', () => {
	test('uses the backend defaults when an older backend omits the keys', () => {
		const policies = ensureAccountDeletionRolePolicies({}, {});

		expect(accountDeletionPolicyKeys).toEqual(['canDeleteAccount', 'canPurgeAccount']);
		expect(accountDeletionPolicyDefaults).toEqual({
			canDeleteAccount: true,
			canPurgeAccount: true,
		});
		expect(policies).toEqual({
			canDeleteAccount: { useDefault: true, priority: 0, value: true },
			canPurgeAccount: { useDefault: true, priority: 0, value: true },
		});
	});

	test('reads explicit base policy values', () => {
		const policies = ensureAccountDeletionRolePolicies({}, {
			canDeleteAccount: false,
			canPurgeAccount: false,
		});

		expect(policies.canDeleteAccount.value).toBe(false);
		expect(policies.canPurgeAccount.value).toBe(false);
	});

	test('preserves assigned-role values and metadata on readback', () => {
		const saved = {
			canDeleteAccount: { useDefault: false, priority: 7, value: false },
			canPurgeAccount: { useDefault: false, priority: 3, value: true },
		};

		const reloaded = ensureAccountDeletionRolePolicies(structuredClone(saved), {
			canDeleteAccount: true,
			canPurgeAccount: true,
		});

		expect(reloaded).toEqual(saved);
	});
});
