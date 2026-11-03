/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, assert, beforeEach, describe, test, vi } from 'vitest';
import type { Mock } from 'vitest';

const state = vi.hoisted(() => ({
	embedded: false,
	instance: {} as Record<string, unknown>,
	fetchInstance: null as unknown as Mock<(force?: boolean) => Promise<unknown>>,
}));

vi.mock('@/instance.js', () => ({
	get instance() {
		return state.instance;
	},
	consumeProvidedMeta: () => state.embedded,
	fetchInstance: (force?: boolean) => state.fetchInstance(force),
}));

import { useWelcomeInstance } from '@/utility/elythia-welcome-instance.js';
import welcomeSource from '@/pages/welcome.vue?raw';

describe('useWelcomeInstance', () => {
	beforeEach(() => {
		state.embedded = false;
		state.instance = {};
		state.fetchInstance = vi.fn(() => Promise.resolve(state.instance));
	});

	afterEach(() => {
		vi.restoreAllMocks();
	});

	// HTMLに埋め込まれたmetaは要求ごとに新しいので、/api/meta を待たずに描き、取り直しもしない
	test('uses the meta embedded in this page without refetching', () => {
		state.embedded = true;
		state.instance = { requireSetup: false, clientOptions: {} };
		const result = useWelcomeInstance();
		assert.strictEqual(result.value, state.instance);
		assert.strictEqual(state.fetchInstance.mock.calls.length, 0);
	});

	// SPAの中で戻ってきたときや手元のキャッシュを採ったときは、描いてから裏で取り直す
	test('draws the loaded instance right away and refetches in the background when it may be stale', () => {
		state.instance = { requireSetup: false, clientOptions: {} };
		const result = useWelcomeInstance();
		assert.strictEqual(result.value, state.instance);
		assert.deepStrictEqual(state.fetchInstance.mock.calls, [[true]]);
	});

	// 何も読み込めていないときは、本家どおり /api/meta を待つ
	test('waits for /api/meta when nothing is loaded', async () => {
		state.embedded = true;
		const fetched = { requireSetup: true, clientOptions: {} };
		state.fetchInstance = vi.fn(() => Promise.resolve(fetched));
		const result = useWelcomeInstance();
		assert.isNull(result.value);
		assert.deepStrictEqual(state.fetchInstance.mock.calls, [[true]]);
		await Promise.resolve();
		assert.strictEqual(result.value, fetched);
	});
});

// 入口の画面そのものが、取り直しを待つ本家の形に戻っていないこと
describe('welcome.vue', () => {
	test('draws with useWelcomeInstance instead of waiting for fetchInstance', () => {
		assert.include(welcomeSource, 'useWelcomeInstance()');
		assert.notInclude(welcomeSource, 'fetchInstance');
	});
});
