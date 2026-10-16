/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Elythia: Service Worker の Static Routing API で、fetch ハンドラーを通す要求を絞る (#3613)。
//
// fetch ハンドラーが応答を作るのは HTML (ナビゲーション) だけで、JS / CSS / 画像 / API は何もせずに
// 返している。それでも全ての要求が SW に渡され、SW が止まっていれば起動を待つ。ナビゲーションと、
// 末尾が / の URL (fetch ハンドラーの isHTMLRequest が HTML とみなすもの) だけを fetch-event に回し、
// 残りは SW を通さずに network へ送る。
//
// 規則を 1 つでも登録すると Chromium の ServiceWorkerAutoPreload から外れる
// (https://issues.chromium.org/issues/466790291)。登録に失敗したときは、以前と同じ
// 「全てを fetch-event に回す」規則に戻して、外れた状態を保つ。
// addRoutes の無いブラウザ (Firefox、Safari) では何もしない (今までどおり全てが fetch ハンドラーを通る)。

/** A URLPattern init dictionary. */
export type URLPatternInit = Record<string, string>;

/** A rule of the Static Routing API (`InstallEvent.addRoutes`). */
export type RouterRule = {
	condition: Record<string, unknown>;
	source: 'fetch-event' | 'network';
};

/** The part of InstallEvent this module uses. `addRoutes` is missing where the API is not supported. */
export type RouteInstallEvent = {
	addRoutes?: (rules: RouterRule | RouterRule[]) => Promise<void>;
};

/**
 * The routes: navigations and URLs ending with `/` go to the fetch handler,
 * everything else to the network.
 */
export function staticRoutes(makePattern: (init: URLPatternInit) => unknown): RouterRule[] {
	return [
		{ condition: { requestMode: 'navigate' }, source: 'fetch-event' },
		// fetch ハンドラーは URL が / で終わる要求も HTML として扱う (sw.ts の isHTMLRequest)。
		// 要求の URL は fragment を含まないので、「/ で終わる」は「パスが / で終わり、クエリが無い」と同じ
		{ condition: { urlPattern: makePattern({ pathname: '*/', search: '' }) }, source: 'fetch-event' },
		{ condition: { urlPattern: makePattern({}) }, source: 'network' },
	];
}

/** The rule registered before #3613: everything goes to the fetch handler. */
export function legacyRoutes(makePattern: (init: URLPatternInit) => unknown): RouterRule[] {
	return [{ condition: { urlPattern: makePattern({}) }, source: 'fetch-event' }];
}

/**
 * Registers the routes during the install event. Returns the promise to pass
 * to `waitUntil`, or null when the browser has no Static Routing API. The
 * promise never rejects, so a failure cannot fail the installation.
 */
export function registerStaticRoutes(ev: RouteInstallEvent, makePattern: (init: URLPatternInit) => unknown): Promise<void> | null {
	const addRoutes = ev.addRoutes;
	if (typeof addRoutes !== 'function') return null;

	const attempt = (rules: () => RouterRule[]): Promise<void> => {
		try {
			return Promise.resolve(addRoutes.call(ev, rules()));
		} catch (err) {
			return Promise.reject(err);
		}
	};

	return attempt(() => staticRoutes(makePattern))
		.catch(() => attempt(() => legacyRoutes(makePattern)))
		.catch(() => { /* empty */ });
}
