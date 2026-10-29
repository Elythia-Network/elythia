/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, describe, expect, test, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/vue';

const mocks = vi.hoisted(() => ({
	account: { federationStatus: undefined as 'pending' | 'on' | 'off' | undefined },
	misskeyApi: vi.fn(),
	apiWithDialog: vi.fn(),
	confirm: vi.fn(),
}));

vi.mock('@/i.js', () => ({ ensureSignin: () => mocks.account }));
vi.mock('@/utility/misskey-api.js', () => ({ misskeyApi: mocks.misskeyApi }));
vi.mock('@/os.js', () => ({ apiWithDialog: mocks.apiWithDialog, confirm: mocks.confirm }));
vi.mock('@/i18n.js', () => ({ i18n: { ts: { _localOnlyAccount: {
	settingsTitle: 'Account federation', settingsDescription: 'Federation settings',
	statusOn: 'Federating', statusOff: 'Local-only', statusPending: 'Unconfirmed',
	systemAccount: 'System account', enableWarning: 'May be irreversible',
	turnOn: 'Enable federation', turnOff: 'Disable federation',
	confirmTurnOn: 'Enable?', confirmTurnOff: 'Disable?',
	reasonMigration: 'Migration', reasonWebFinger: 'WebFinger', reasonActor: 'Actor',
	reasonCollection: 'Collection', reasonNote: 'Note', reasonInbox: 'Inbox',
	reasonDelivery: 'Delivery', reasonUnknown: 'Other',
} }, tsx: { _localOnlyAccount: { firstReference: () => 'First reference' } } } }));
vi.mock('@/components/MkButton.vue', () => ({
	default: { props: ['disabled'], emits: ['click'], template: '<button :disabled="disabled" @click="$emit(\'click\')"><slot /></button>' },
}));
vi.mock('@/components/global/MkError.vue', () => ({
	default: { emits: ['retry'], template: '<button data-testid="retry" @click="$emit(\'retry\')">Retry</button>' },
}));
vi.mock('@/components/global/MkLoading.vue', () => ({ default: { template: '<div data-testid="loading">Loading</div>' } }));
vi.mock('@/components/MkInfo.vue', () => ({ default: { template: '<div><slot /></div>' } }));
vi.mock('@/components/MkKeyValue.vue', () => ({ default: { template: '<div><slot name="key" /><slot name="value" /></div>' } }));

import FederationSettings from '@/pages/settings/federation.vue';

describe('federation settings loading', () => {
	afterEach(() => {
		cleanup();
		vi.clearAllMocks();
		mocks.account.federationStatus = undefined;
	});

	test('keeps controls hidden after load failure and exposes them after retry succeeds', async () => {
		mocks.misskeyApi
			.mockRejectedValueOnce(new Error('network'))
			.mockResolvedValueOnce({
				federationStatus: 'on',
				firstExternalReferenceAt: null,
				firstExternalReferenceReason: null,
				firstExternalReferenceHost: null,
				isSystemAccount: false,
			});
		const view = render(FederationSettings);

		await waitFor(() => expect(view.getByTestId('retry')).toBeTruthy());
		expect(view.queryByText('Enable federation')).toBeNull();
		expect(view.queryByText('Disable federation')).toBeNull();
		expect(mocks.account.federationStatus).toBeUndefined();
		expect(mocks.apiWithDialog).not.toHaveBeenCalled();

		await fireEvent.click(view.getByTestId('retry'));
		await waitFor(() => expect(view.getByText('Disable federation')).toBeTruthy());
		expect(mocks.account.federationStatus).toBe('on');
		expect(mocks.apiWithDialog).not.toHaveBeenCalled();
	});
});
