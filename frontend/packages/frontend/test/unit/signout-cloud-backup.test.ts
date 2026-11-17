/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

const mocks = vi.hoisted(() => ({
	cloudBackup: vi.fn(async () => {}),
	unisonReload: vi.fn(),
	waiting: vi.fn(),
	store: { s: { enablePreferencesAutoCloudBackup: true } },
}));

vi.mock('@/preferences/utility.js', () => ({ cloudBackup: mocks.cloudBackup }));
vi.mock('@/store.js', () => ({ store: mocks.store }));
vi.mock('@/os.js', () => ({ waiting: mocks.waiting }));
vi.mock('@/utility/unison-reload.js', () => ({ unisonReload: mocks.unisonReload }));
vi.mock('@/utility/idb-proxy.js', () => ({ clear: async () => {} }));
vi.mock('@/i.js', () => ({ $i: { id: 'user', token: 'revoked' } }));

import { signout } from '@/signout.js';

beforeEach(() => {
	localStorage.setItem('account', JSON.stringify({ id: 'user', token: 'revoked' }));
});

afterEach(() => {
	localStorage.clear();
	mocks.cloudBackup.mockReset();
	mocks.cloudBackup.mockImplementation(async () => {});
	mocks.unisonReload.mockReset();
	mocks.store.s.enablePreferencesAutoCloudBackup = true;
	vi.restoreAllMocks();
});

describe('signout', () => {
	test('finishes signing out when the automatic cloud backup fails', async () => {
		const error = vi.spyOn(console, 'error').mockImplementation(() => {});
		mocks.cloudBackup.mockImplementation(async () => { throw new Error('AUTHENTICATION_FAILED'); });

		await expect(signout()).resolves.toBeUndefined();

		expect(mocks.cloudBackup).toHaveBeenCalledTimes(1);
		expect(error).toHaveBeenCalled();
		expect(localStorage.getItem('account')).toBeNull();
		expect(mocks.unisonReload).toHaveBeenCalledWith('/');
	});

	test('backs up before clearing the storage when the backup succeeds', async () => {
		let accountAtBackup: string | null = null;
		mocks.cloudBackup.mockImplementation(async () => { accountAtBackup = localStorage.getItem('account'); });

		await signout();

		expect(accountAtBackup).not.toBeNull();
		expect(localStorage.getItem('account')).toBeNull();
		expect(mocks.unisonReload).toHaveBeenCalledWith('/');
	});

	test('does not back up when the automatic cloud backup is off', async () => {
		mocks.store.s.enablePreferencesAutoCloudBackup = false;

		await signout();

		expect(mocks.cloudBackup).not.toHaveBeenCalled();
		expect(mocks.unisonReload).toHaveBeenCalledWith('/');
	});
});
