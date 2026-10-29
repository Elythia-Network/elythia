/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

export type FederationStatus = 'pending' | 'off' | 'on';

export type FederationReferenceReasonLabelKey =
	| 'reasonMigration'
	| 'reasonWebFinger'
	| 'reasonActor'
	| 'reasonCollection'
	| 'reasonNote'
	| 'reasonInbox'
	| 'reasonDelivery'
	| 'reasonUnknown';

export function shouldShowLocalOnlyProfileNotice(host: string | null | undefined, status: FederationStatus | undefined): boolean {
	return host == null && (status === 'off' || status === 'pending');
}

export function federationReferenceReasonLabelKey(reason: string | null | undefined): FederationReferenceReasonLabelKey {
	switch (reason) {
		case 'migration': return 'reasonMigration';
		case 'webfinger': return 'reasonWebFinger';
		case 'actor': return 'reasonActor';
		case 'collection': return 'reasonCollection';
		case 'note': return 'reasonNote';
		case 'inbox': return 'reasonInbox';
		case 'delivery': return 'reasonDelivery';
		default: return 'reasonUnknown';
	}
}

export async function confirmFederationDisable(
	current: FederationStatus,
	next: FederationStatus,
	confirm: () => Promise<boolean>,
): Promise<boolean> {
	return current === 'on' && next === 'off' ? confirm() : true;
}

export type FederationReferenceState = {
	federationStatus: FederationStatus;
	firstExternalReferenceAt: string | null;
	isSystemAccount: boolean;
};

export function canDisableFederation(state: FederationReferenceState): boolean {
	return state.federationStatus === 'on' && state.firstExternalReferenceAt == null && !state.isSystemAccount;
}

export function canUseAccountMigration(status: FederationStatus | undefined): boolean {
	return status !== 'off' && status !== 'pending';
}

export function shouldBlockRemoteFederationAction(status: FederationStatus | undefined, isRemoteTarget: boolean): boolean {
	return (status === 'off' || status === 'pending') && isRemoteTarget;
}

export function resolveLocalOnlyPreference(status: FederationStatus | undefined, requested: boolean | undefined): boolean {
	return status === 'off' ? true : requested ?? false;
}

export async function updateFederationWithRefresh<T>(update: () => Promise<T>, refresh: () => Promise<void>): Promise<T> {
	try {
		return await update();
	} catch (error) {
		await refresh();
		throw error;
	}
}

export async function finalizeFederationSetup(options: {
	current: FederationStatus | undefined;
	choice: 'on' | 'off';
	confirmFederation?: () => Promise<boolean>;
	update: (status: 'on' | 'off') => Promise<FederationStatus>;
	complete: () => void;
}): Promise<boolean> {
	if (options.current === 'pending') {
		if (options.choice === 'on' && options.confirmFederation && !await options.confirmFederation()) return false;
		await options.update(options.choice);
	}
	options.complete();
	return true;
}
