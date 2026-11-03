/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it, vi } from 'vitest';

// 初めての訪問で、/api/emojis の要求を JS の読み込みと並べて始める (#3993)。
// boot.js は module ではないので、該当の region を取り出して動かす。

const HOUR = 1000 * 60 * 60;

type IdbState =
	| { kind: 'missing' }
	| { kind: 'present'; values: Record<string, unknown> }
	| { kind: 'throws' };

// idb-keyval の既定の store (keyval-store / keyval) だけを持つ、最小の IndexedDB。
// DB が無いときは onupgradeneeded を出し、abort されたら onerror を出す (仕様どおり DB は残らない)。
function fakeIndexedDB(state: IdbState) {
	const calls = { opened: [] as string[], aborted: 0, closed: 0, created: 0 };
	const indexedDB = {
		open(name: string) {
			if (state.kind === 'throws') throw new Error('SecurityError');
			calls.opened.push(name);
			const req: any = { result: undefined, transaction: undefined, onupgradeneeded: null, onsuccess: null, onerror: null };
			queueMicrotask(() => {
				if (state.kind === 'missing' || name !== 'keyval-store') {
					let aborted = false;
					req.result = { createObjectStore: () => { calls.created++; } };
					req.transaction = { abort: () => { aborted = true; calls.aborted++; } };
					req.onupgradeneeded?.({});
					if (aborted) {
						req.onerror?.({ preventDefault: () => {} });
						return;
					}
				}
				const values = state.kind === 'present' ? state.values : {};
				req.result = {
					close: () => { calls.closed++; },
					transaction: (store: string, mode: string) => {
						if (store !== 'keyval' || mode !== 'readonly') throw new Error('NotFoundError');
						return {
							objectStore: (s: string) => ({
								get: (key: string) => {
									if (s !== 'keyval') throw new Error('NotFoundError');
									const getReq: any = { result: undefined, onsuccess: null, onerror: null };
									queueMicrotask(() => {
										getReq.result = values[key];
										getReq.onsuccess?.({});
									});
									return getReq;
								},
							}),
						};
					},
				};
				req.onsuccess?.({});
			});
			return req;
		},
	};
	return { indexedDB, calls };
}

function fakeLocalStorage(items: Record<string, string>) {
	return { getItem: (k: string) => (k in items ? items[k] : null) };
}

function runPrefetchRegion(opts: {
	idb: IdbState;
	localStorage?: Record<string, string>;
	fetch?: (...args: unknown[]) => Promise<unknown>;
}) {
	const source = readFileSync(join(__dirname, '../../public/loader/boot.js'), 'utf8');
	const match = source.match(/\/\/#region Prefetch emojis\n([\s\S]*?)\/\/#endregion/);
	if (match == null) throw new Error('Prefetch emojis region not found in boot.js');
	const { indexedDB, calls } = fakeIndexedDB(opts.idb);
	const fetch = vi.fn(opts.fetch ?? (async () => ({ status: 200, json: async () => ({ emojis: [] }) })));
	const fakeWindow: Record<string, unknown> = { fetch, location: { origin: 'https://local.example' } };
	new Function('window', 'indexedDB', 'localStorage', match[1])(
		fakeWindow, indexedDB, fakeLocalStorage(opts.localStorage ?? {}),
	);
	return { result: fakeWindow.__elythia_emojis_prefetch__ as Promise<unknown>, fetch, calls };
}

const okResponse = (body: unknown) => async () => ({ status: 200, json: async () => body });

describe('boot.js emoji prefetch', () => {
	it('requests the list on the first visit without creating the database', async () => {
		const body = { emojis: [{ name: 'a', url: '/a.png', category: null, aliases: [] }] };
		const r = runPrefetchRegion({ idb: { kind: 'missing' }, fetch: okResponse(body) });
		await expect(r.result).resolves.toEqual(body);
		expect(r.fetch).toHaveBeenCalledTimes(1);
		// misskeyApiGet('emojis', {}) と同じ要求
		expect(r.fetch).toHaveBeenCalledWith('https://local.example/api/emojis?', { method: 'GET', credentials: 'omit', cache: 'default' });
		expect(r.calls.opened).toEqual(['keyval-store']);
		expect(r.calls.aborted).toBe(1);
		expect(r.calls.created).toBe(0);
	});

	it('requests the list when the last fetch is older than an hour', async () => {
		const r = runPrefetchRegion({ idb: { kind: 'present', values: { lastEmojisFetchedAt: Date.now() - 2 * HOUR } } });
		await expect(r.result).resolves.toEqual({ emojis: [] });
		expect(r.fetch).toHaveBeenCalledTimes(1);
		expect(r.calls.closed).toBe(1);
	});

	it('requests the list when the database has no timestamp', async () => {
		const r = runPrefetchRegion({ idb: { kind: 'present', values: {} } });
		await expect(r.result).resolves.toEqual({ emojis: [] });
		expect(r.fetch).toHaveBeenCalledTimes(1);
	});

	it('does not request a list fetched within the hour', async () => {
		const r = runPrefetchRegion({ idb: { kind: 'present', values: { lastEmojisFetchedAt: Date.now() - 10 * 60 * 1000 } } });
		await expect(r.result).resolves.toBeNull();
		expect(r.fetch).not.toHaveBeenCalled();
		expect(r.calls.closed).toBe(1);
	});

	it('reads the localStorage fallback of idb-proxy when IndexedDB is unavailable', async () => {
		const fresh = runPrefetchRegion({
			idb: { kind: 'throws' },
			localStorage: { 'idbfallback::lastEmojisFetchedAt': JSON.stringify(Date.now() - 60 * 1000) },
		});
		await expect(fresh.result).resolves.toBeNull();
		expect(fresh.fetch).not.toHaveBeenCalled();

		const stale = runPrefetchRegion({
			idb: { kind: 'throws' },
			localStorage: { 'idbfallback::lastEmojisFetchedAt': JSON.stringify(Date.now() - 2 * HOUR) },
		});
		await expect(stale.result).resolves.toEqual({ emojis: [] });
		expect(stale.fetch).toHaveBeenCalledTimes(1);

		const none = runPrefetchRegion({ idb: { kind: 'throws' } });
		await expect(none.result).resolves.toEqual({ emojis: [] });
		expect(none.fetch).toHaveBeenCalledTimes(1);
	});

	it.each([
		['a network error', async () => { throw new TypeError('Failed to fetch'); }],
		['a 5xx response', async () => ({ status: 502, json: async () => ({ error: {} }) })],
		// misskeyApiGet と同じく 200 以外は使わない (中身が一覧の形でも)
		['a non-200 response with a list-shaped body', async () => ({ status: 500, json: async () => ({ emojis: [] }) })],
		['a broken JSON body', async () => ({ status: 200, json: async () => { throw new SyntaxError('Unexpected token'); } })],
		['a body without the list', async () => ({ status: 200, json: async () => ({ error: {} }) })],
		['a null body', async () => ({ status: 200, json: async () => null })],
	])('resolves to null instead of rejecting on %s', async (_, fetch) => {
		const r = runPrefetchRegion({ idb: { kind: 'missing' }, fetch });
		await expect(r.result).resolves.toBeNull();
	});
});
