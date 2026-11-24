/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Elythia: Service Worker が受ける要求を決める。規則と判定は本家の sw.ts と同じで、
// /flush だけを Service Worker に通さない。
//
// fetch ハンドラーは遷移を fetch(request) で送り直す。送り直すと Sec-Fetch-Mode が
// navigate から same-origin に変わり、Sec-Fetch-Site も Service Worker から見た値になるので、
// サーバー (internal/server/flush.go) が、ページとしての遷移か、別のサイトから
// 遷移させられたかを見分けられなくなる。/flush はブラウザが自分でネットワークへ送る。

/** The page that clears what the client stores (served by internal/server/flush.go). */
export const FLUSH_PATHNAME = '/flush';

/** A URLPattern init dictionary. */
export type URLPatternInit = Record<string, string>;

/** A rule of the Static Routing API (`InstallEvent.addRoutes`). */
export type RouterRule = {
	condition: Record<string, unknown>;
	source: 'fetch-event' | 'network';
};

/**
 * The rules registered at install. The first matching rule wins: /flush goes
 * to the network, navigations and URLs ending with `/` to the fetch handler,
 * everything else to the network.
 */
export function routerRules(makePattern: (init: URLPatternInit) => unknown): RouterRule[] {
	return [
		{ condition: { urlPattern: makePattern({ pathname: FLUSH_PATHNAME }) }, source: 'network' },
		{ condition: { requestMode: 'navigate' }, source: 'fetch-event' },
		// fetchハンドラーはURLが/で終わるリクエストもHTMLとして扱う (クエリが付くと/では終わらない)
		{ condition: { urlPattern: makePattern({ pathname: '*/', search: '' }) }, source: 'fetch-event' },
		// ルールは先に一致したものが優先されるので、これは上記以外のすべてに当たる
		{ condition: { urlPattern: makePattern({}) }, source: 'network' },
	];
}

/**
 * The rule for browsers that reject {@link routerRules}: everything goes to
 * the fetch handler, which still leaves /flush alone ({@link respondsToFetch}).
 */
export function fallbackRules(makePattern: (init: URLPatternInit) => unknown): RouterRule[] {
	return [{ condition: { urlPattern: makePattern({}) }, source: 'fetch-event' }];
}

/**
 * Whether the fetch handler answers the request (with respondWith). It answers
 * HTML requests only, and never /flush, so the browser sends that one itself.
 */
export function respondsToFetch(request: Pick<Request, 'url' | 'headers'>): boolean {
	if (new URL(request.url).pathname === FLUSH_PATHNAME) return false;
	if (request.headers.get('sec-fetch-dest') === 'document') return true;
	if (request.headers.get('accept')?.includes('/html')) return true;
	return request.url.endsWith('/');
}
