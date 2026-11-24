/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, it } from 'vitest';
import { fallbackRules, respondsToFetch, routerRules } from '../../../sw/src/scripts/routes.js';
import type { RouterRule, URLPatternInit } from '../../../sw/src/scripts/routes.js';

// URLPattern は Node 24 から。手元の古い Node でも回せるよう、規則が使う形だけを真似る。
// 実物との突き合わせは、URLPattern がある Node (CI) で下の 'real URLPattern' が行う。
type FakePattern = { test(url: string): boolean };
const fakePattern = (init: URLPatternInit): FakePattern => ({
	test(url: string) {
		const u = new URL(url);
		if (init.search !== undefined && u.search.replace(/^\?/, '') !== init.search) return false;
		if (init.pathname === undefined) return true;
		if (init.pathname.startsWith('*')) return u.pathname.endsWith(init.pathname.slice(1));
		return u.pathname === init.pathname;
	},
});

// 先に一致した規則の source を返す (Static Routing API の決め方)。
function routeOf(rules: RouterRule[], url: string, mode: RequestMode): RouterRule['source'] | undefined {
	for (const rule of rules) {
		const { requestMode, urlPattern } = rule.condition as { requestMode?: string; urlPattern?: FakePattern };
		if (requestMode !== undefined && requestMode !== mode) continue;
		if (urlPattern !== undefined && !urlPattern.test(url)) continue;
		return rule.source;
	}
	return undefined;
}

const routeCases = [
	['https://example.test/flush', 'navigate', 'network'],
	['https://example.test/flush?x=1', 'navigate', 'network'],
	['https://example.test/', 'navigate', 'fetch-event'],
	['https://example.test/notes/abc', 'navigate', 'fetch-event'],
	['https://example.test/flushx', 'navigate', 'fetch-event'],
	['https://example.test/a/flush', 'navigate', 'fetch-event'],
	['https://example.test/admin/', 'cors', 'fetch-event'],
	['https://example.test/api/i', 'cors', 'network'],
	['https://example.test/vite/a.js', 'no-cors', 'network'],
] as const;

describe('routerRules', () => {
	const rules = routerRules(fakePattern);

	it.each(routeCases)('%s (%s) goes to %s', (url, mode, want) => {
		expect(routeOf(rules, url, mode)).toBe(want);
	});

	it('the fallback sends everything to the fetch handler', () => {
		expect(routeOf(fallbackRules(fakePattern), 'https://example.test/api/i', 'cors')).toBe('fetch-event');
	});

	const URLPatternImpl = (globalThis as { URLPattern?: new (init: URLPatternInit) => FakePattern }).URLPattern;
	it('URLPattern is available on CI', () => {
		if (process.env.CI) expect(URLPatternImpl).toBeTypeOf('function');
	});
	it.skipIf(!URLPatternImpl)('real URLPattern routes the same (skipped where Node has no URLPattern)', () => {
		const real = routerRules(init => new URLPatternImpl!(init));
		for (const [url, mode, want] of routeCases) {
			expect(routeOf(real, url, mode), url).toBe(want);
		}
	});
});

describe('respondsToFetch', () => {
	const req = (url: string, headers: Record<string, string> = {}) => ({ url, headers: new Headers(headers) });

	it.each([
		['a navigation', req('https://example.test/notes/abc', { 'sec-fetch-dest': 'document' }), true],
		['an HTML request', req('https://example.test/notes/abc', { accept: 'text/html' }), true],
		['a URL ending with /', req('https://example.test/admin/'), true],
		['an API request', req('https://example.test/api/i', { accept: 'application/json' }), false],
		['a navigation to /flush', req('https://example.test/flush', { 'sec-fetch-dest': 'document', accept: 'text/html' }), false],
		['/flush with a query', req('https://example.test/flush?x=1', { 'sec-fetch-dest': 'document' }), false],
		['a navigation to /flushx', req('https://example.test/flushx', { 'sec-fetch-dest': 'document' }), true],
		['a navigation to /a/flush', req('https://example.test/a/flush', { 'sec-fetch-dest': 'document' }), true],
	])('%s', (_label, request, want) => {
		expect(respondsToFetch(request)).toBe(want);
	});
});
