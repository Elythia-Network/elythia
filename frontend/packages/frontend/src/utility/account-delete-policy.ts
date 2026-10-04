/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

export const accountDeletionPolicyDefaults = {
	canDeleteAccount: true,
	canPurgeAccount: true,
} as const;

export const accountDeletionPolicyKeys = ['canDeleteAccount', 'canPurgeAccount'] as const;

type AccountDeletionPolicyKey = typeof accountDeletionPolicyKeys[number];

export type AccountDeletionRolePolicy = {
	useDefault: boolean;
	priority: number;
	value: unknown;
};

export function isAccountDeletionAllowed(policies: object): boolean {
	// Older backends do not return this key. Keep their existing self-delete UI;
	// the supporting backend still enforces typed true at the API boundary.
	return (policies as Record<string, unknown>).canDeleteAccount !== false;
}

export function shouldShowAccountDeletionSection(policies: object, isDeleted: boolean): boolean {
	return isDeleted || isAccountDeletionAllowed(policies);
}

export function ensureAccountDeletionRolePolicies<T extends Record<string, AccountDeletionRolePolicy>>(
	policies: T,
	basePolicies: object,
): T & Record<AccountDeletionPolicyKey, AccountDeletionRolePolicy> {
	const base = basePolicies as Record<string, unknown>;
	const extended = policies as T & Partial<Record<AccountDeletionPolicyKey, AccountDeletionRolePolicy>>;
	for (const key of accountDeletionPolicyKeys) {
		if (extended[key] == null) {
			const baseValue = base[key];
			extended[key] = {
				useDefault: true,
				priority: 0,
				value: typeof baseValue === 'boolean' ? baseValue : accountDeletionPolicyDefaults[key],
			};
		}
	}
	return extended as T & Record<AccountDeletionPolicyKey, AccountDeletionRolePolicy>;
}
