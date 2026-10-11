/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/*
 * Maintenance mode support for navigations (Elythia #3463).
 *
 * サーバーはメンテナンス中、目印のヘッダーを付けた 503 と自前の画面を返す。
 * SW は 5xx を全部オフラインの画面に差し替えるので、目印の付いた 503 だけは
 * そのまま返し、画面をキャッシュしておく。本体が作り直しの途中で一瞬応答しない
 * 場合や、前段の proxy が 502 を返す場合に、オフラインの画面ではなくキャッシュした
 * メンテナンスの画面を出すため。
 */

/** Header the server sets on every maintenance response. */
export const MAINTENANCE_HEADER = 'x-elythia-maintenance';
/** Cache holding the last maintenance page. Kept across activations. */
export const MAINTENANCE_CACHE = 'elythia-maintenance';
/** Cache key of the maintenance page. */
export const MAINTENANCE_PAGE_KEY = '/__elythia_maintenance__';
/** How long a remembered maintenance stays trusted. */
export const MAINTENANCE_MAX_AGE_MS = 6 * 60 * 60 * 1000;

/** Storage the maintenance helpers use; the SW wires caches and IndexedDB. */
export type MaintenanceStore = {
	openCache: () => Promise<Pick<Cache, 'put' | 'match'>>;
	deleteCache: () => Promise<unknown>;
	getMark: () => Promise<number | undefined>;
	setMark: (at: number) => Promise<void>;
	delMark: () => Promise<void>;
	now: () => number;
};

/**
 * Whether activate keeps the cache named name. The locale cache of this
 * version and the maintenance page survive; everything else is dropped.
 */
export function keepCacheOnActivate(name: string, langCacheName: string): boolean {
	return name === langCacheName || name === MAINTENANCE_CACHE;
}

/** Whether res is the server's maintenance answer. */
export function isMaintenanceResponse(res: Response | undefined): boolean {
	return res?.status === 503 && res.headers.get(MAINTENANCE_HEADER) === '1';
}

/** Keeps a copy of the maintenance page and remembers when it was seen. */
export async function rememberMaintenance(res: Response, store: MaintenanceStore): Promise<void> {
	const cache = await store.openCache();
	await cache.put(MAINTENANCE_PAGE_KEY, res.clone());
	await store.setMark(store.now());
}

/** Forgets the maintenance after a normal navigation answer. */
export async function forgetMaintenance(store: MaintenanceStore): Promise<void> {
	if (await store.getMark() === undefined) return;
	await store.delMark();
	await store.deleteCache();
}

/**
 * Returns the cached maintenance page when a maintenance was seen recently,
 * or undefined (then the caller shows the offline page).
 */
export async function cachedMaintenancePage(store: MaintenanceStore): Promise<Response | undefined> {
	const mark = await store.getMark();
	if (mark === undefined || store.now() - mark > MAINTENANCE_MAX_AGE_MS) return undefined;
	const cache = await store.openCache();
	return await cache.match(MAINTENANCE_PAGE_KEY) ?? undefined;
}

/**
 * Decides the answer to a navigation from what fetch gave.
 *
 * - the maintenance 503 is returned as is (and remembered)
 * - any other answer below 500, or an opaque redirect, is returned as is
 *   (and the maintenance is forgotten)
 * - a failed fetch or another 5xx shows the cached maintenance page when a
 *   maintenance was seen recently; otherwise undefined, for the offline page
 */
export async function maintenanceAwareNavigation(
	fetched: Response | undefined,
	store: MaintenanceStore,
	method = 'GET',
): Promise<Response | undefined> {
	if (fetched && isMaintenanceResponse(fetched)) {
		// 覚えるのは GET の画面だけ。メンテナンスの画面は終わったかを HEAD で
		// 確かめることがあり、その本文の無い応答で画面のキャッシュを上書きしない。
		if (method === 'GET') {
			await rememberMaintenance(fetched, store).catch(() => undefined);
		}
		return fetched;
	}
	if (fetched && ((fetched.status && fetched.status < 500) || fetched.type === 'opaqueredirect')) {
		await forgetMaintenance(store).catch(() => undefined);
		return fetched;
	}
	return await cachedMaintenancePage(store).catch(() => undefined);
}
