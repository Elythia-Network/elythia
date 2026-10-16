/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// `test/setup-network.unit.ts` が `globalThis.fetch` に入れる fetch の本体。テストから
// 直接試せるよう分けている (`fetchMock` を通すと、中止済みの signal は `fetchMock` が
// 先に同期で投げるので、ここの分岐まで届かない)。

export type HoldingFetchState = {
	/** True while the test file is being collected; requests then go to the real fetch. */
	collecting: () => boolean;
	/** Counts of requests (`METHOD url`) held during the current test. */
	pending: () => Map<string, number>;
};

function requestUrl(input: RequestInfo | URL): URL {
	const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
	return new URL(raw, window.location.href);
}

function isApiRequest(url: URL): boolean {
	return url.origin === window.location.origin && url.pathname.startsWith('/api/');
}

/**
 * Creates a fetch that holds same-origin `/api/` requests without answering them
 * (rejecting only when aborted) and passes everything else to `realFetch`.
 */
export function createHoldingFetch(realFetch: typeof globalThis.fetch, state: HoldingFetchState): typeof globalThis.fetch {
	return ((input: RequestInfo | URL, init?: RequestInit) => {
		const url = requestUrl(input);
		if (state.collecting() || !isApiRequest(url)) return realFetch(input, init);
		const signal = init?.signal ?? (input instanceof Request ? input.signal : undefined);
		const method = (init?.method ?? (input instanceof Request ? input.method : 'GET')).toUpperCase();
		const label = `${method} ${url.href}`;
		const pending = state.pending();
		// 同じ method と URL の要求が同時に待つことがあるので、数で持つ。片方を中止しても
		// もう片方は一覧に残す。
		const hold = () => pending.set(label, (pending.get(label) ?? 0) + 1);
		const release = () => {
			const count = (pending.get(label) ?? 0) - 1;
			if (count > 0) pending.set(label, count);
			else pending.delete(label);
		};
		return new Promise<Response>((_resolve, reject) => {
			if (signal == null) {
				hold();
				return;
			}
			const abort = () => {
				release();
				reject(signal.reason ?? new DOMException('The operation was aborted.', 'AbortError'));
			};
			if (signal.aborted) {
				abort();
				return;
			}
			hold();
			signal.addEventListener('abort', abort, { once: true });
		});
	}) as typeof globalThis.fetch;
}
