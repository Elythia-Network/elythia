/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ログイントークンの再生成 (pages/settings/security.vue) の流れ (#4277)。本家 2026.11.1
// (#18162) と同じく、自動バックアップを再生成より先に済ませ、再生成の後にサインアウトする。
// Elythia は再生成の失敗を画面に出し (apiWithDialog)、失敗したらサインアウトしない。

import { afterEach, describe, expect, test, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/vue';

const mocks = vi.hoisted(() => ({
	calls: [] as string[],
	authenticateDialog: vi.fn(),
	waiting: vi.fn(),
	apiWithDialog: vi.fn(),
	cloudBackup: vi.fn(),
	signout: vi.fn(),
	store: { s: { enablePreferencesAutoCloudBackup: true } },
	// i18n.ts.<key> を key の名前にする (入れ子の _settings なども同じ)
	keyProxy: (): Record<string, any> => new Proxy({}, { get: (_, k) => (k === '_settings' ? mocks.keyProxy() : String(k)) }),
}));

vi.mock('@/os.js', () => ({
	authenticateDialog: mocks.authenticateDialog,
	waiting: mocks.waiting,
	apiWithDialog: mocks.apiWithDialog,
}));
vi.mock('@/preferences/utility.js', () => ({ cloudBackup: mocks.cloudBackup }));
vi.mock('@/signout.js', () => ({ signout: mocks.signout }));
vi.mock('@/store.js', () => ({ store: mocks.store }));
vi.mock('@/i18n.js', () => ({ i18n: { ts: mocks.keyProxy() } }));
vi.mock('@/page.js', () => ({ definePage: () => {} }));
vi.mock('@/utility/paginator.js', () => ({ Paginator: class {} }));
vi.mock('@/pages/settings/2fa.vue', () => ({ default: { template: '<div />' } }));
vi.mock('@/components/MkPagination.vue', () => ({ default: { template: '<div />' } }));
vi.mock('@/components/MkFeatureBanner.vue', () => ({ default: { template: '<div><slot /></div>' } }));
vi.mock('@/components/form/section.vue', () => ({ default: { template: '<div><slot /></div>' } }));
vi.mock('@/components/form/slot.vue', () => ({ default: { template: '<div><slot /></div>' } }));
vi.mock('@/components/MkButton.vue', () => ({
	default: { emits: ['click'], template: '<button @click="$emit(\'click\')"><slot /></button>' },
}));

import XSecurity from '@/pages/settings/security.vue';

// SearchMarker などはビルドの変換が渡す属性 (children など) を受けるので、div に移さない
const passthrough = { inheritAttrs: false, template: '<div><slot /></div>' };

async function pressRegenerate() {
	const view = render(XSecurity, {
		global: { stubs: { SearchMarker: passthrough, SearchLabel: passthrough, SearchText: passthrough, MkTime: true }, directives: { panel: {} } },
	});
	await fireEvent.click(view.getByText('regenerateLoginToken'));
}

describe('settings/security: regenerate the login token', () => {
	afterEach(() => {
		cleanup();
		vi.clearAllMocks();
		mocks.calls.length = 0;
		mocks.store.s.enablePreferencesAutoCloudBackup = true;
	});

	function setup(regenerate: () => Promise<unknown>) {
		mocks.authenticateDialog.mockResolvedValue({ canceled: false, result: { password: 'pw', token: null } });
		mocks.waiting.mockReturnValue(() => {});
		mocks.cloudBackup.mockImplementation(async () => { mocks.calls.push('cloudBackup'); });
		mocks.apiWithDialog.mockImplementation(async () => { mocks.calls.push('regenerate'); return regenerate(); });
		mocks.signout.mockImplementation(async () => { mocks.calls.push('signout'); });
	}

	test('backs up before regenerating, then signs out', async () => {
		setup(async () => undefined);
		await pressRegenerate();
		await waitFor(() => expect(mocks.signout).toHaveBeenCalledTimes(1));
		expect(mocks.calls).toEqual(['cloudBackup', 'regenerate', 'signout']);
		expect(mocks.apiWithDialog).toHaveBeenCalledWith('i/regenerate-token', { password: 'pw', token: null });
	});

	test('does not sign out when regenerating fails', async () => {
		setup(async () => { throw new Error('INCORRECT_PASSWORD'); });
		await pressRegenerate();
		await waitFor(() => expect(mocks.apiWithDialog).toHaveBeenCalledTimes(1));
		// apiWithDialog の失敗の後に続きが走る機会を与えてから確かめる
		await new Promise((r) => setTimeout(r, 0));
		expect(mocks.signout).not.toHaveBeenCalled();
	});

	test('does nothing when the authentication dialog is canceled', async () => {
		setup(async () => undefined);
		mocks.authenticateDialog.mockResolvedValue({ canceled: true });
		await pressRegenerate();
		await new Promise((r) => setTimeout(r, 0));
		expect(mocks.apiWithDialog).not.toHaveBeenCalled();
		expect(mocks.signout).not.toHaveBeenCalled();
	});
});
