/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, describe, expect, test, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/vue';

const mocks = vi.hoisted(() => ({
	set: vi.fn(),
	confirm: vi.fn(),
	apiWithDialog: vi.fn(),
	popupAsyncWithDialog: vi.fn(),
	dispose: vi.fn(),
	close: vi.fn(),
	account: { federationStatus: 'pending' as 'pending' | 'on' | 'off', policies: { canSearchNotes: true } },
}));

vi.mock('@/store.js', () => ({ store: { s: { accountSetupWizard: 5 }, set: mocks.set } }));
vi.mock('@/i.js', () => ({
	$i: mocks.account,
	iAmModerator: false,
	iAmAdmin: false,
	notesCount: 0,
	incNotesCount: () => {},
	ensureSignin: () => mocks.account,
}));
vi.mock('@/os.js', () => ({ confirm: mocks.confirm, apiWithDialog: mocks.apiWithDialog, popupAsyncWithDialog: mocks.popupAsyncWithDialog }));
vi.mock('@/i18n.js', () => ({ i18n: { ts: {
	_localOnlyAccount: {
		setupTitle: 'Federation settings', setupDescription: 'Choose federation',
		enableFederation: 'Federate', setupOnWarning: 'External copies may remain.', setupOffDescription: 'Posts stay here.',
	},
	_initialAccountSetting: { skipAreYouSure: 'Skip?', initialAccountSettingCompleted: 'Setup completed', startTutorial: 'Start tutorial' },
	initialAccountSetting: 'Setup', goBack: 'Back', continue: 'Continue',
	}, tsx: { _initialAccountSetting: { youCanContinueTutorial: () => 'Tutorial is available' } } } }));
vi.mock('@/instance.js', () => ({ instance: { name: 'Elythia' } }));
vi.mock('@/components/MkModalWindow.vue', async () => {
	const { defineComponent, h } = await import('vue');
	return { default: defineComponent({
		emits: ['close', 'closed'],
		setup(_, { emit, slots, expose }) {
			expose({ close: mocks.close });
			return () => h('div', [h('button', { 'data-testid': 'dialog-close', onClick: () => emit('close') }, 'Close'), slots.header?.(), slots.default?.()]);
		},
	}) };
});
vi.mock('@/components/MkUserSetupDialog.Profile.vue', () => ({ default: { template: '<div />' } }));
vi.mock('@/components/MkUserSetupDialog.Follow.vue', () => ({ default: { template: '<div />' } }));
vi.mock('@/components/MkUserSetupDialog.Privacy.vue', () => ({ default: { template: '<div />' } }));
vi.mock('@/components/MkAnimBg.vue', () => ({ default: { template: '<div />' } }));
vi.mock('@/components/MkPushNotificationAllowButton.vue', () => ({ default: { template: '<div />' } }));
vi.mock('@/components/MkInfo.vue', () => ({ default: { template: '<div><slot /></div>' } }));
vi.mock('@/components/MkButton.vue', async () => {
	const { defineComponent, h } = await import('vue');
	return { default: defineComponent({
		emits: ['click'],
		setup(_, { emit, attrs, slots }) { return () => h('button', { ...attrs, onClick: () => emit('click') }, slots.default?.()); },
	}) };
});
vi.mock('@/components/MkSwitch.vue', () => ({ default: { props: ['modelValue'], emits: ['update:modelValue'], template: '<input type="checkbox" :checked="modelValue" @change="$emit(\'update:modelValue\', $event.target.checked)">' } }));

import MkUserSetupDialog from '@/components/MkUserSetupDialog.vue';

describe('MkUserSetupDialog federation decision', () => {
	afterEach(() => {
		cleanup();
		vi.clearAllMocks();
		mocks.account.federationStatus = 'pending';
	});

	test('normal Continue applies OFF and reaches the completion page before closing', async () => {
		mocks.apiWithDialog.mockResolvedValue({ federationStatus: 'off' });
		const view = render(MkUserSetupDialog, { props: {}, global: { stubs: { MkInfo: true } } });
		await fireEvent.click(view.getByRole('checkbox'));
		await fireEvent.click(view.getByTestId('user-setup-finish'));
		await waitFor(() => expect(view.getByText('Setup completed')).toBeTruthy());
		expect(mocks.apiWithDialog).toHaveBeenCalledWith('i/update-federation', { federationStatus: 'off' });
		expect(mocks.set).toHaveBeenCalledWith('accountSetupWizard', 6);
		expect(mocks.set).not.toHaveBeenCalledWith('accountSetupWizard', -1);
		await fireEvent.click(view.getByTestId('user-setup-close'));
		expect(mocks.set).toHaveBeenCalledWith('accountSetupWizard', -1);
	});

	test('skip forces ON even after OFF was selected', async () => {
		mocks.confirm.mockResolvedValue({ canceled: false });
		mocks.apiWithDialog.mockResolvedValue({ federationStatus: 'on' });
		const view = render(MkUserSetupDialog, { props: {}, global: { stubs: { MkInfo: true } } });
		await fireEvent.click(view.getByRole('checkbox'));
		await fireEvent.click(view.getByTestId('dialog-close'));
		await waitFor(() => expect(mocks.set).toHaveBeenCalledWith('accountSetupWizard', -1));
		expect(mocks.apiWithDialog).toHaveBeenCalledWith('i/update-federation', { federationStatus: 'on' });
	});

	test('completion page retains the start-tutorial action', async () => {
		mocks.confirm.mockResolvedValue({ canceled: false });
		mocks.apiWithDialog.mockResolvedValue({ federationStatus: 'on' });
		mocks.popupAsyncWithDialog.mockResolvedValue({ dispose: mocks.dispose });
		const view = render(MkUserSetupDialog, { props: {}, global: { stubs: { MkInfo: true } } });
		await fireEvent.click(view.getByTestId('user-setup-finish'));
		await waitFor(() => expect(view.getByText('Setup completed')).toBeTruthy());
		await fireEvent.click(view.getByTestId('user-setup-tutorial'));
		await waitFor(() => expect(mocks.popupAsyncWithDialog).toHaveBeenCalledOnce());
		expect(mocks.set).toHaveBeenCalledWith('accountSetupWizard', -1);
	});

	test('API failure leaves the wizard on the federation choice page', async () => {
		mocks.confirm.mockResolvedValue({ canceled: false });
		mocks.apiWithDialog.mockRejectedValue(new Error('network'));
		const view = render(MkUserSetupDialog, { props: {}, global: { stubs: { MkInfo: true } } });
		await fireEvent.click(view.getByTestId('user-setup-finish'));
		await waitFor(() => expect(mocks.apiWithDialog).toHaveBeenCalledOnce());
		expect(view.getByTestId('user-setup-finish')).toBeTruthy();
		expect(view.queryByText('Setup completed')).toBeNull();
		expect(mocks.set).not.toHaveBeenCalledWith('accountSetupWizard', -1);
		expect(mocks.set).not.toHaveBeenCalledWith('accountSetupWizard', 6);
		expect(mocks.close).not.toHaveBeenCalled();
	});
});
