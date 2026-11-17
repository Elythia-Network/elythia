/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';

const AUTHENTICATION_FAILED = 'b0a7f5f8-dc2f-4171-b91f-de88ad238e14';
const CREDENTIAL_REQUIRED = '1384574d-a912-4b81-8601-c7b1c4085df1';
const YOUR_ACCOUNT_SUSPENDED = 'a8c724b3-6e9c-4b46-b1a8-bc3ed6258370';
const USER_IS_DELETED = 'e5b3b9f0-2b8f-4b9f-9c1f-8c5c1b2e1b1a';

const mocks = vi.hoisted(() => ({
	signout: vi.fn(),
	unisonReload: vi.fn(),
	alert: vi.fn(async () => {}),
	store: {
		s: { accountTokens: {} as Record<string, string>, accountInfos: {} as Record<string, unknown> },
		set(key: 'accountTokens' | 'accountInfos', value: any) { this.s[key] = value; },
	},
	prefer: {
		s: { accounts: [] as [string, { id: string; username: string }][] },
		commit(key: 'accounts', value: any) { this.s[key] = value; },
	},
	me: { id: 'current', token: 'current-token' },
}));

vi.mock('@@/js/config.js', () => ({ apiUrl: 'https://example.test/api', host: 'example.test' }));
vi.mock('@/utility/show-suspended-dialog.js', () => ({ showSuspendedDialog: vi.fn(async () => {}) }));
vi.mock('@/i18n.js', () => ({ i18n: { ts: {} } }));
vi.mock('@/os.js', () => ({
	alert: mocks.alert,
	popup: () => ({ dispose: () => {} }),
	popupMenu: vi.fn(),
	success: vi.fn(),
	waiting: vi.fn(),
}));
vi.mock('@/utility/unison-reload.js', () => ({ unisonReload: mocks.unisonReload, reloadChannel: { postMessage: vi.fn() } }));
vi.mock('@/preferences.js', () => ({ prefer: mocks.prefer }));
vi.mock('@/store.js', () => ({ store: mocks.store }));
vi.mock('@/i.js', () => ({ $i: mocks.me }));
vi.mock('@/signout.js', () => ({ signout: mocks.signout }));

import { refreshCurrentAccount } from '@/accounts.js';

// tokenごとに/api/iの応答を決める。表に無いtokenは、`<id>-token`のidの有効なアカウントとして返す。
function stubFetch(responses: Record<string, 'revoked' | 'suspended' | 'deleted' | 'credential-required' | 'server-error' | 'network-error'>) {
	const fetch = vi.fn(async (_url: string, init: { body: string }) => {
		const { i } = JSON.parse(init.body) as { i: string };
		const r = responses[i];
		if (r === 'network-error') throw new TypeError('Failed to fetch');
		if (r === 'server-error') return new Response('', { status: 502 });
		if (r === 'revoked') {
			return new Response(JSON.stringify({ error: { id: AUTHENTICATION_FAILED } }), { status: 401 });
		}
		if (r === 'deleted') {
			return new Response(JSON.stringify({ error: { id: USER_IS_DELETED } }), { status: 403 });
		}
		if (r === 'suspended') {
			return new Response(JSON.stringify({ error: { id: YOUR_ACCOUNT_SUSPENDED } }), { status: 403 });
		}
		// Elythiaのbackendは、トークンの解決がDBの一時的な失敗で止まったときにこれを返す。
		if (r === 'credential-required') {
			return new Response(JSON.stringify({ error: { id: CREDENTIAL_REQUIRED } }), { status: 401 });
		}
		const id = i.replace(/-token$/, '');
		return new Response(JSON.stringify({ id, username: id }), { status: 200 });
	});
	vi.stubGlobal('fetch', fetch);
	return fetch;
}

function registerAccounts(ids: string[]) {
	mocks.store.s.accountTokens = Object.fromEntries(ids.map(id => [`example.test/${id}`, `${id}-token`]));
	mocks.store.s.accountInfos = Object.fromEntries(ids.map(id => [`example.test/${id}`, { id }]));
	mocks.prefer.s.accounts = ids.map(id => ['example.test', { id, username: id }]);
}

function registeredIds() {
	return mocks.prefer.s.accounts.map(([, user]) => user.id);
}

beforeEach(() => {
	vi.spyOn(console, 'error').mockImplementation(() => {});
});

afterEach(() => {
	localStorage.clear();
	mocks.signout.mockReset();
	mocks.unisonReload.mockReset();
	mocks.alert.mockClear();
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
});

describe('refreshCurrentAccount with a revoked token', () => {
	test('drops every other revoked account and signs out when none is left', async () => {
		registerAccounts(['current', 'second', 'third']);
		stubFetch({ 'current-token': 'revoked', 'second-token': 'revoked', 'third-token': 'revoked' });

		await refreshCurrentAccount();

		expect(registeredIds()).toEqual([]);
		expect(mocks.store.s.accountTokens).toEqual({});
		expect(mocks.signout).toHaveBeenCalledTimes(1);
		expect(mocks.unisonReload).not.toHaveBeenCalled();
	});

	test('skips a revoked account and switches to the next usable one', async () => {
		registerAccounts(['current', 'second', 'third']);
		const fetch = stubFetch({ 'current-token': 'revoked', 'second-token': 'revoked' });

		await refreshCurrentAccount();

		expect(fetch.mock.calls.map(([, init]) => JSON.parse(init.body).i)).toEqual(['current-token', 'second-token', 'third-token']);
		expect(registeredIds()).toEqual(['third']);
		expect(JSON.parse(localStorage.getItem('account')!).token).toBe('third-token');
		expect(mocks.unisonReload).toHaveBeenCalledTimes(1);
		expect(mocks.signout).not.toHaveBeenCalled();
	});

	test('signs out when the revoked account was the only one', async () => {
		registerAccounts(['current']);
		stubFetch({ 'current-token': 'revoked' });

		await refreshCurrentAccount();

		expect(registeredIds()).toEqual([]);
		expect(mocks.signout).toHaveBeenCalledTimes(1);
	});

	test('keeps an account it could not check because of a server error', async () => {
		registerAccounts(['current', 'second', 'third']);
		stubFetch({ 'current-token': 'revoked', 'second-token': 'server-error', 'third-token': 'revoked' });

		await refreshCurrentAccount();

		expect(registeredIds()).toEqual(['second', 'third']);
		expect(mocks.signout).not.toHaveBeenCalled();
		expect(mocks.unisonReload).not.toHaveBeenCalled();
	});

	test('keeps an account it could not check because of a network error', async () => {
		registerAccounts(['current', 'second', 'third']);
		stubFetch({ 'current-token': 'revoked', 'second-token': 'network-error', 'third-token': 'revoked' });

		await refreshCurrentAccount();

		expect(registeredIds()).toEqual(['second', 'third']);
		expect(mocks.signout).not.toHaveBeenCalled();
	});

	test('keeps an account that fails with CREDENTIAL_REQUIRED', async () => {
		registerAccounts(['current', 'second', 'third']);
		stubFetch({ 'current-token': 'revoked', 'second-token': 'credential-required', 'third-token': 'revoked' });

		await refreshCurrentAccount();

		expect(registeredIds()).toEqual(['second', 'third']);
		expect(mocks.signout).not.toHaveBeenCalled();
		expect(mocks.unisonReload).not.toHaveBeenCalled();
	});

	test.each(['suspended', 'deleted'] as const)('drops a %s account like a revoked one and moves on', async (state) => {
		registerAccounts(['current', 'second', 'third']);
		stubFetch({ 'current-token': 'revoked', 'second-token': state });

		await refreshCurrentAccount();

		expect(registeredIds()).toEqual(['third']);
		expect(mocks.unisonReload).toHaveBeenCalledTimes(1);
	});

	test('leaves everything as it is when the current account fails with CREDENTIAL_REQUIRED', async () => {
		registerAccounts(['current', 'second']);
		const fetch = stubFetch({ 'current-token': 'credential-required', 'second-token': 'revoked' });

		await refreshCurrentAccount();

		expect(fetch).toHaveBeenCalledTimes(1);
		expect(registeredIds()).toEqual(['current', 'second']);
		expect(mocks.signout).not.toHaveBeenCalled();
	});

	test('leaves everything as it is when the current account fails with a server error', async () => {
		registerAccounts(['current', 'second']);
		const fetch = stubFetch({ 'current-token': 'server-error', 'second-token': 'revoked' });

		await refreshCurrentAccount();

		expect(fetch).toHaveBeenCalledTimes(1);
		expect(registeredIds()).toEqual(['current', 'second']);
		expect(mocks.signout).not.toHaveBeenCalled();
	});
});
