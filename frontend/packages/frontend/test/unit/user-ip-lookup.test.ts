/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test, vi } from 'vitest';
import {
	adminShowUserParams,
	createLazyUserIpLookup,
	ipLookupLogRedirect,
	normalizeModerationLogTab,
} from '@/utility/user-ip-lookup.js';

function deferred<T>() {
	let resolve!: (value: T) => void;
	let reject!: (reason?: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

describe('adminShowUserParams', () => {
	test('mk-go ではページ表示時の signin 取得を明示的に止める', () => {
		expect(adminShowUserParams('user-a', '1.0.0')).toEqual({
			userId: 'user-a',
			withSignins: false,
		});
	});

	test('純正 backend には未知のパラメータを送らない', () => {
		expect(adminShowUserParams('user-a', null)).toEqual({ userId: 'user-a' });
		expect(adminShowUserParams('user-a', '')).toEqual({ userId: 'user-a' });
	});
});

describe('createLazyUserIpLookup', () => {
	test('初期表示では取得せず、最初に開いた時だけ取得する', async () => {
		const fetcher = vi.fn(async () => [{ ip: 'example', createdAt: 'now' }]);
		const lookup = createLazyUserIpLookup('user-a', fetcher);

		expect(lookup.status.value).toBe('idle');
		expect(fetcher).not.toHaveBeenCalled();

		await lookup.load();
		expect(fetcher).toHaveBeenCalledOnce();
		expect(fetcher).toHaveBeenCalledWith('user-a');
		expect(lookup.status.value).toBe('loaded');
		expect(lookup.value.value).toEqual([{ ip: 'example', createdAt: 'now' }]);
	});

	test('再展開では成功結果を再利用する', async () => {
		const fetcher = vi.fn(async () => ['loaded']);
		const lookup = createLazyUserIpLookup('user-a', fetcher);

		await lookup.load();
		await lookup.load();

		expect(fetcher).toHaveBeenCalledOnce();
	});

	test('読み込み中の再展開は同じ request を共有する', async () => {
		const pending = deferred<string[]>();
		const fetcher = vi.fn(() => pending.promise);
		const lookup = createLazyUserIpLookup('user-a', fetcher);

		const first = lookup.load();
		const second = lookup.load();
		expect(second).toBe(first);
		expect(fetcher).toHaveBeenCalledOnce();

		pending.resolve(['loaded']);
		await first;
		expect(lookup.status.value).toBe('loaded');
	});

	test('ユーザー切替後は古い応答を捨て、新しいユーザーを取得できる', async () => {
		const first = deferred<string[]>();
		const second = deferred<string[]>();
		const fetcher = vi.fn((userId: string) => userId === 'user-a' ? first.promise : second.promise);
		const lookup = createLazyUserIpLookup('user-a', fetcher);

		const oldRequest = lookup.load();
		lookup.reset('user-b');
		expect(lookup.status.value).toBe('idle');
		const newRequest = lookup.load();

		first.resolve(['old']);
		await oldRequest;
		expect(lookup.value.value).toBe(null);

		second.resolve(['new']);
		await newRequest;
		expect(lookup.value.value).toEqual(['new']);
		expect(fetcher).toHaveBeenNthCalledWith(2, 'user-b');
	});

	test('同じユーザーでresetしても古い応答を捨てる', async () => {
		const first = deferred<string[]>();
		const second = deferred<string[]>();
		const fetcher = vi.fn()
			.mockReturnValueOnce(first.promise)
			.mockReturnValueOnce(second.promise);
		const lookup = createLazyUserIpLookup('user-a', fetcher);

		const oldRequest = lookup.load();
		lookup.reset('user-a');
		const newRequest = lookup.load();

		first.resolve(['old']);
		await oldRequest;
		expect(lookup.status.value).toBe('loading');
		expect(lookup.value.value).toBe(null);

		second.resolve(['new']);
		await newRequest;
		expect(lookup.status.value).toBe('loaded');
		expect(lookup.value.value).toEqual(['new']);
	});

	test('古いリクエストの失敗を新しいユーザーの状態へ反映しない', async () => {
		const first = deferred<string[]>();
		const second = deferred<string[]>();
		const fetcher = vi.fn((userId: string) => userId === 'user-a' ? first.promise : second.promise);
		const lookup = createLazyUserIpLookup('user-a', fetcher);

		const oldRequest = lookup.load();
		lookup.reset('user-b');
		const newRequest = lookup.load();

		const oldError = new Error('old request failed');
		first.reject(oldError);
		await oldRequest;
		expect(lookup.status.value).toBe('loading');
		expect(lookup.error.value).toBe(null);

		second.resolve(['new']);
		await newRequest;
		expect(lookup.status.value).toBe('loaded');
		expect(lookup.value.value).toEqual(['new']);
	});

	test('古いリクエストの完了後も3回目は進行中のリクエストを共有する', async () => {
		const first = deferred<string[]>();
		const second = deferred<string[]>();
		const fetcher = vi.fn()
			.mockReturnValueOnce(first.promise)
			.mockReturnValueOnce(second.promise);
		const lookup = createLazyUserIpLookup('user-a', fetcher);

		const oldRequest = lookup.load();
		lookup.reset('user-b');
		const currentRequest = lookup.load();

		first.resolve(['old']);
		await oldRequest;
		const thirdRequest = lookup.load();

		expect(thirdRequest).toBe(currentRequest);
		expect(fetcher).toHaveBeenCalledTimes(2);

		second.resolve(['new']);
		await thirdRequest;
		expect(lookup.value.value).toEqual(['new']);
	});

	test('同期的な失敗を保持し、明示的な再試行を許す', async () => {
		const error = new Error('unavailable');
		const fetcher = vi.fn()
			.mockImplementationOnce(() => { throw error; })
			.mockResolvedValueOnce(['loaded']);
		const lookup = createLazyUserIpLookup('user-a', fetcher);

		await lookup.load();
		expect(lookup.status.value).toBe('error');
		expect(lookup.error.value).toBe(error);
		expect(fetcher).toHaveBeenCalledOnce();

		await lookup.retry();
		expect(fetcher).toHaveBeenCalledTimes(2);
		expect(lookup.status.value).toBe('loaded');
		expect(lookup.error.value).toBe(null);
	});
});

describe('normalizeModerationLogTab', () => {
	test('権限がある場合だけ IP 監査タブを選べる', () => {
		expect(normalizeModerationLogTab('ipLookup', true)).toBe('ipLookup');
		expect(normalizeModerationLogTab('ipLookup', false)).toBe('moderation');
	});

	test('未知の deep link はモデレーションログへ戻す', () => {
		expect(normalizeModerationLogTab('unknown', true)).toBe('moderation');
		expect(normalizeModerationLogTab(undefined, true)).toBe('moderation');
	});
});

describe('ipLookupLogRedirect', () => {
	test('旧URLのqueryやhashを引き継がない関数redirectを返す', () => {
		expect(ipLookupLogRedirect()).toBe('/admin/modlog#ipLookup');
	});
});
