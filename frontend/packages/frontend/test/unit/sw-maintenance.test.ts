/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, it } from 'vitest';
import {
	MAINTENANCE_CACHE,
	MAINTENANCE_HEADER,
	MAINTENANCE_MAX_AGE_MS,
	keepCacheOnActivate,
	maintenanceAwareNavigation,
} from '../../../sw/src/scripts/maintenance.js';
import type { MaintenanceStore } from '../../../sw/src/scripts/maintenance.js';

// SW の caches と IndexedDB の代わり。
function memoryStore(start = 1_000_000) {
	const cache = new Map<string, Response>();
	const state = { mark: undefined as number | undefined, now: start, cacheDeleted: 0 };
	const store: MaintenanceStore = {
		openCache: async () => ({
			put: async (key: RequestInfo | URL, res: Response) => { cache.set(String(key), res); },
			match: async (key: RequestInfo | URL) => cache.get(String(key))?.clone(),
		}) as Pick<Cache, 'put' | 'match'>,
		deleteCache: async () => { cache.clear(); state.cacheDeleted++; return true; },
		getMark: async () => state.mark,
		setMark: async (at) => { state.mark = at; },
		delMark: async () => { state.mark = undefined; },
		now: () => state.now,
	};
	return { store, cache, state };
}

function maintenancePage() {
	return new Response('<p>maintenance</p>', {
		status: 503,
		headers: { [MAINTENANCE_HEADER]: '1', 'content-type': 'text/html' },
	});
}

describe('maintenanceAwareNavigation', () => {
	it('passes the marked 503 through instead of the offline page, and remembers it', async () => {
		const { store, cache, state } = memoryStore();
		const res = maintenancePage();
		const answer = await maintenanceAwareNavigation(res, store);
		expect(answer).toBe(res);
		expect(state.mark).toBe(state.now);
		expect(cache.size).toBe(1);
	});

	it('does not overwrite the cached page with the answer to a HEAD', async () => {
		const { store, cache, state } = memoryStore();
		await maintenanceAwareNavigation(maintenancePage(), store);
		const markedAt = state.mark;
		state.now += 60_000;
		const head = new Response(null, { status: 503, headers: { [MAINTENANCE_HEADER]: '1' } });
		const answer = await maintenanceAwareNavigation(head, store, 'HEAD');
		expect(answer).toBe(head);
		expect(state.mark).toBe(markedAt);
		expect(await cache.get('/__elythia_maintenance__')?.clone().text()).toBe('<p>maintenance</p>');
	});

	it('shows the cached maintenance page when the server cannot be reached', async () => {
		const { store, state } = memoryStore();
		await maintenanceAwareNavigation(maintenancePage(), store);
		state.now += 60_000;
		// 通信の失敗 (fetch が投げた) と、前段の proxy の 502 の両方。
		for (const fetched of [undefined, new Response('bad gateway', { status: 502 })]) {
			const answer = await maintenanceAwareNavigation(fetched, store);
			expect(answer?.status).toBe(503);
			expect(await answer?.text()).toBe('<p>maintenance</p>');
		}
	});

	it('falls back to the offline page once the remembered maintenance is too old', async () => {
		const { store, state } = memoryStore();
		await maintenanceAwareNavigation(maintenancePage(), store);
		state.now += MAINTENANCE_MAX_AGE_MS + 1;
		expect(await maintenanceAwareNavigation(undefined, store)).toBeUndefined();
	});

	it('falls back to the offline page when no maintenance was seen', async () => {
		const { store } = memoryStore();
		expect(await maintenanceAwareNavigation(undefined, store)).toBeUndefined();
		expect(await maintenanceAwareNavigation(new Response('', { status: 500 }), store)).toBeUndefined();
	});

	it('does not treat an unmarked 503 as the maintenance', async () => {
		const { store, state } = memoryStore();
		expect(await maintenanceAwareNavigation(new Response('', { status: 503 }), store)).toBeUndefined();
		expect(state.mark).toBeUndefined();
	});

	it('forgets the maintenance after a normal answer', async () => {
		const { store, cache, state } = memoryStore();
		await maintenanceAwareNavigation(maintenancePage(), store);
		const ok = new Response('<html></html>', { status: 200 });
		expect(await maintenanceAwareNavigation(ok, store)).toBe(ok);
		expect(state.mark).toBeUndefined();
		expect(cache.size).toBe(0);
		expect(await maintenanceAwareNavigation(undefined, store)).toBeUndefined();
		// 覚えていないときは、毎回キャッシュを消しに行かない。
		await maintenanceAwareNavigation(new Response('', { status: 404 }), store);
		expect(state.cacheDeleted).toBe(1);
	});

	it('still answers when the storage fails', async () => {
		const { store } = memoryStore();
		const broken: MaintenanceStore = {
			...store,
			openCache: async () => { throw new Error('quota'); },
			getMark: async () => { throw new Error('idb'); },
		};
		const res = maintenancePage();
		expect(await maintenanceAwareNavigation(res, broken)).toBe(res);
		const ok = new Response('', { status: 200 });
		expect(await maintenanceAwareNavigation(ok, broken)).toBe(ok);
		expect(await maintenanceAwareNavigation(undefined, broken)).toBeUndefined();
	});
});

describe('keepCacheOnActivate', () => {
	it('keeps the locale cache and the maintenance page only', () => {
		expect(keepCacheOnActivate('mk-cache-1.0.0', 'mk-cache-1.0.0')).toBe(true);
		expect(keepCacheOnActivate(MAINTENANCE_CACHE, 'mk-cache-1.0.0')).toBe(true);
		expect(keepCacheOnActivate('mk-cache-0.9.0', 'mk-cache-1.0.0')).toBe(false);
	});
});
