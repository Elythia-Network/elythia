/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { shallowRef } from 'vue';

export type UserIpLookupStatus = 'idle' | 'loading' | 'loaded' | 'error';
export type ModerationLogTab = 'moderation' | 'ipLookup';

/**
 * mk-go accepts an additive opt-out which prevents admin/show-user from
 * reading sign-in IPs. Pure upstream backends may reject unknown parameters,
 * so their request stays byte-for-byte compatible.
 */
export function adminShowUserParams(userId: string, mkGoVersion: string | null | undefined):
	{ userId: string } | { userId: string; withSignins: false } {
	return mkGoVersion == null || mkGoVersion === ''
		? { userId }
		: { userId, withSignins: false };
}

/**
 * Keep unauthorized and malformed deep links on the ordinary moderation log.
 * The caller uses this before mounting the IP log, so normalization also
 * prevents an unauthorized audit-log request.
 */
export function normalizeModerationLogTab(requested: string | undefined, canViewIpLookup: boolean): ModerationLogTab {
	return requested === 'ipLookup' && canViewIpLookup ? 'ipLookup' : 'moderation';
}

/**
 * A function redirect prevents Nirax from appending the legacy URL's query or
 * hash after `#ipLookup`, which would turn the tab key into an invalid value.
 */
export function ipLookupLogRedirect(): string {
	return '/admin/modlog#ipLookup';
}

/**
 * A per-user lazy loader for the protected admin/get-user-ips request.
 * Successful results are cached, concurrent opens share one promise, errors
 * remain retryable, and reset() invalidates late responses from the old user.
 */
export function createLazyUserIpLookup<T>(initialUserId: string, fetcher: (userId: string) => Promise<T>) {
	const status = shallowRef<UserIpLookupStatus>('idle');
	const value = shallowRef<T | null>(null);
	const error = shallowRef<unknown | null>(null);
	let userId = initialUserId;
	let generation = 0;
	let inFlight: Promise<void> | null = null;

	function reset(nextUserId: string): void {
		generation++;
		userId = nextUserId;
		inFlight = null;
		status.value = 'idle';
		value.value = null;
		error.value = null;
	}

	function load(): Promise<void> {
		if (status.value === 'loaded') return Promise.resolve();
		if (inFlight != null) return inFlight;

		const requestedUserId = userId;
		const requestedGeneration = generation;
		status.value = 'loading';
		error.value = null;

		let fetched: Promise<T>;
		try {
			fetched = fetcher(requestedUserId);
		} catch (reason) {
			fetched = Promise.reject(reason);
		}

		const request = Promise.resolve(fetched)
			.then(result => {
				if (requestedGeneration !== generation || requestedUserId !== userId) return;
				value.value = result;
				status.value = 'loaded';
			})
			.catch(reason => {
				if (requestedGeneration !== generation || requestedUserId !== userId) return;
				value.value = null;
				error.value = reason;
				status.value = 'error';
			})
			.finally(() => {
				if (inFlight === request) inFlight = null;
			});

		inFlight = request;
		return request;
	}

	function retry(): Promise<void> {
		return load();
	}

	return {
		status,
		value,
		error,
		load,
		retry,
		reset,
	};
}
