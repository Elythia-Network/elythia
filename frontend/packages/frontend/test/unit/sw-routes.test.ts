/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, it, vi } from 'vitest';
import { legacyRoutes, registerStaticRoutes, staticRoutes } from '../../../sw/src/scripts/routes.js';
import type { RouterRule } from '../../../sw/src/scripts/routes.js';

// SW の Static Routing で、ナビゲーション以外を network に回す (#3613)。
const pattern = (init: Record<string, string>) => ({ pattern: init });

describe('staticRoutes', () => {
	it('sends navigations and URLs ending with / to the fetch handler, the rest to the network', () => {
		expect(staticRoutes(pattern)).toEqual([
			{ condition: { requestMode: 'navigate' }, source: 'fetch-event' },
			{ condition: { urlPattern: { pattern: { pathname: '*/', search: '' } } }, source: 'fetch-event' },
			{ condition: { urlPattern: { pattern: {} } }, source: 'network' },
		]);
	});

	it('the catch-all network rule comes last (the first matching rule wins)', () => {
		const rules = staticRoutes(pattern);
		expect(rules.at(-1)?.source).toBe('network');
		expect(rules.slice(0, -1).every(r => r.source === 'fetch-event')).toBe(true);
	});

	// 実際の URLPattern で、規則が想定どおりの URL に当たるか。URLPattern は Node 24 から。
	// CI (frontend/.node-version の Node) では必ず走らせ、無ければ落とす。手元の古い Node でだけ skip する。
	const hasURLPattern = typeof (globalThis as { URLPattern?: unknown }).URLPattern === 'function';
	it('URLPattern is available on CI', () => {
		if (process.env.CI) expect(hasURLPattern).toBe(true);
	});
	it.skipIf(!hasURLPattern)('the / rule matches like isHTMLRequest (skipped where Node has no URLPattern)', () => {
		const URLPatternImpl = (globalThis as unknown as { URLPattern: new (init: Record<string, string>) => { test(url: string): boolean } }).URLPattern;
		const slash = new URLPatternImpl({ pathname: '*/', search: '' });
		for (const url of ['https://example.test/', 'https://example.test/admin/']) {
			expect(slash.test(url), url).toBe(url.endsWith('/'));
		}
		for (const url of ['https://example.test/api/i', 'https://example.test/a/?q=1', 'https://example.test/vite/ja-JP/a.js']) {
			expect(slash.test(url), url).toBe(false);
		}
	});
});

describe('registerStaticRoutes', () => {
	it('does nothing without the Static Routing API', () => {
		expect(registerStaticRoutes({}, pattern)).toBeNull();
	});

	it('registers the new routes', async () => {
		const addRoutes = vi.fn(async (_: RouterRule | RouterRule[]) => {});
		await registerStaticRoutes({ addRoutes }, pattern);
		expect(addRoutes).toHaveBeenCalledTimes(1);
		expect(addRoutes).toHaveBeenCalledWith(staticRoutes(pattern));
	});

	it('falls back to the previous rule when the browser rejects the new routes', async () => {
		const addRoutes = vi.fn()
			.mockRejectedValueOnce(new TypeError('unsupported condition'))
			.mockResolvedValueOnce(undefined);
		await registerStaticRoutes({ addRoutes }, pattern);
		expect(addRoutes).toHaveBeenNthCalledWith(2, legacyRoutes(pattern));
	});

	it('falls back on a synchronous throw and never rejects', async () => {
		const addRoutes = vi.fn(() => { throw new TypeError('bad'); });
		await expect(registerStaticRoutes({ addRoutes }, pattern)).resolves.toBeUndefined();
		expect(addRoutes).toHaveBeenCalledTimes(2);
		expect(addRoutes).toHaveBeenNthCalledWith(2, legacyRoutes(pattern));
	});
});
