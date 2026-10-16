/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * A component loaded with a dynamic import that can be fetched ahead of time
 * and then used synchronously.
 */
export type PreloadableComponent<T> = {
	/** The component once loaded, otherwise null. */
	readonly loaded: T | null;
	/** Starts loading (once) and resolves to the component. A failed load calls the loader again on the next call. */
	preload(): Promise<T>;
};

export function createPreloadableComponent<T>(load: () => Promise<{ default: T }>): PreloadableComponent<T> {
	let loaded: T | null = null;
	let pending: Promise<T> | null = null;
	return {
		get loaded() {
			return loaded;
		},
		preload() {
			if (loaded != null) return Promise.resolve(loaded);
			if (pending == null) {
				pending = load().then(m => {
					loaded = m.default;
					return m.default;
				}, err => {
					// 読み込みに失敗したら、次の呼び出しで import() をやり直す。ただしブラウザは
					// 取得に失敗したモジュールをページの寿命の間覚えているので、実際にはページを
					// 読み込み直すまで同じ失敗になる (呼び出し側で再読み込みを促す)
					pending = null;
					throw err;
				});
			}
			return pending;
		},
	};
}

/**
 * Runs `fn` when the browser is idle (or soon, where requestIdleCallback is missing).
 */
export function runWhenIdle(fn: () => void, timeout = 5000): void {
	if (typeof window.requestIdleCallback === 'function') {
		window.requestIdleCallback(() => fn(), { timeout });
	} else {
		window.setTimeout(fn, 1);
	}
}
