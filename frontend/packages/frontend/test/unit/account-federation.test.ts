/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test, vi } from 'vitest';
import {
	canDisableFederation,
	confirmFederationDisable,
	federationReferenceReasonLabelKey,
	shouldShowLocalOnlyProfileNotice,
	canUseAccountMigration,
	finalizeFederationSetup,
	resolveLocalOnlyPreference,
	updateFederationWithRefresh,
	shouldBlockRemoteFederationAction,
} from '@/utility/account-federation.js';

describe('canDisableFederation', () => {
	test('allows turning federation off only before the first external reference', () => {
		expect(canDisableFederation({ federationStatus: 'on', firstExternalReferenceAt: null, isSystemAccount: false })).toBe(true);
		expect(canDisableFederation({ federationStatus: 'on', firstExternalReferenceAt: '2026-10-26T00:00:00Z', isSystemAccount: false })).toBe(false);
		expect(canDisableFederation({ federationStatus: 'off', firstExternalReferenceAt: null, isSystemAccount: false })).toBe(false);
		expect(canDisableFederation({ federationStatus: 'on', firstExternalReferenceAt: null, isSystemAccount: true })).toBe(false);
	});
});

describe('account federation UI restrictions', () => {
	test('blocks account migration until federation is on', () => {
		expect(canUseAccountMigration('pending')).toBe(false);
		expect(canUseAccountMigration('off')).toBe(false);
		expect(canUseAccountMigration('on')).toBe(true);
	});

	test.each([
		['missing status preserves legacy ON behavior for remote targets', undefined, true, false],
		['missing status allows local targets', undefined, false, false],
		['ON allows remote targets', 'on', true, false],
		['ON allows local targets', 'on', false, false],
		['OFF blocks remote targets', 'off', true, true],
		['OFF allows local targets', 'off', false, false],
		['pending blocks remote targets', 'pending', true, true],
		['pending allows local targets', 'pending', false, false],
	] as const)('%s', (_label, status, remote, expected) => {
		expect(shouldBlockRemoteFederationAction(status, remote)).toBe(expected);
	});

	test('keeps missing-state migration and local-only defaults compatible', () => {
		expect(canUseAccountMigration(undefined)).toBe(true);
		expect(resolveLocalOnlyPreference(undefined, undefined)).toBe(false);
		expect(resolveLocalOnlyPreference(undefined, true)).toBe(true);
	});

	test('keeps local-only preference forced after restoring drafts and initial notes', () => {
		expect(resolveLocalOnlyPreference('off', false)).toBe(true);
		expect(resolveLocalOnlyPreference('off', undefined)).toBe(true);
		expect(resolveLocalOnlyPreference('pending', false)).toBe(false);
	});
});

describe('finalizeFederationSetup', () => {
	test('uses the same completion path for finish and skip, and leaves setup open after failure', async () => {
		const update = vi.fn(async (status: 'on' | 'off') => status);
		const complete = vi.fn();
		const confirmFederation = vi.fn(async () => true);
		await finalizeFederationSetup({ current: 'pending', choice: 'on', confirmFederation, update, complete });
		expect(update).toHaveBeenCalledWith('on');
		expect(complete).toHaveBeenCalledOnce();

		update.mockClear();
		complete.mockClear();
		await finalizeFederationSetup({ current: 'pending', choice: 'on', confirmFederation, update, complete }); // skip defaults to ON
		expect(update).toHaveBeenCalledWith('on');
		expect(complete).toHaveBeenCalledOnce();

		complete.mockClear();
		await expect(finalizeFederationSetup({ current: 'pending', choice: 'off', update: async () => { throw new Error('network'); }, complete })).rejects.toThrow('network');
		expect(complete).not.toHaveBeenCalled();
	});

	test('does not change an already configured federation state', async () => {
		const update = vi.fn(async () => 'on' as const);
		const complete = vi.fn();
		await finalizeFederationSetup({ current: 'off', choice: 'on', update, complete });
		expect(update).not.toHaveBeenCalled();
		expect(complete).toHaveBeenCalledOnce();
	});
});

describe('updateFederationWithRefresh', () => {
	test('refreshes observed state after a stale update is rejected', async () => {
		const refresh = vi.fn(async () => {});
		await expect(updateFederationWithRefresh(async () => { throw new Error('referenced'); }, refresh)).rejects.toThrow('referenced');
		expect(refresh).toHaveBeenCalledOnce();
	});
});

describe('local-only account presentation', () => {
	test('shows the profile notice for local OFF and pending accounts only', () => {
		expect(shouldShowLocalOnlyProfileNotice(null, 'off')).toBe(true);
		expect(shouldShowLocalOnlyProfileNotice(null, 'pending')).toBe(true);
		expect(shouldShowLocalOnlyProfileNotice(null, 'on')).toBe(false);
		expect(shouldShowLocalOnlyProfileNotice('remote.example', 'off')).toBe(false);
	});

	test.each([
		['migration', 'reasonMigration'],
		['webfinger', 'reasonWebFinger'],
		['actor', 'reasonActor'],
		['collection', 'reasonCollection'],
		['note', 'reasonNote'],
		['inbox', 'reasonInbox'],
		['delivery', 'reasonDelivery'],
	] as const)('maps external reference reason %s to a localized label', (reason, label) => {
		expect(federationReferenceReasonLabelKey(reason)).toBe(label);
	});

	test('uses a safe label for an unknown external reference reason', () => {
		expect(federationReferenceReasonLabelKey('future-reason')).toBe('reasonUnknown');
	});

	test('does not submit ON-to-OFF when the irreversible change is canceled', async () => {
		const confirm = vi.fn(async () => false);
		const update = vi.fn();
		const confirmed = await confirmFederationDisable('on', 'off', confirm);
		if (confirmed) update();
		expect(confirmed).toBe(false);
		expect(confirm).toHaveBeenCalledOnce();
		expect(update).not.toHaveBeenCalled();
	});

	test('does not ask for irreversible OFF confirmation when enabling or turning off pending', async () => {
		const confirm = vi.fn(async () => false);
		expect(await confirmFederationDisable('off', 'on', confirm)).toBe(true);
		expect(await confirmFederationDisable('pending', 'off', confirm)).toBe(true);
		expect(confirm).not.toHaveBeenCalled();
	});
});
