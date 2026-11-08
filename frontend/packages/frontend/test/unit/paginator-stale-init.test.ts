/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, beforeEach, vi } from 'vitest';

const h = vi.hoisted(() => ({ api: vi.fn() }));
vi.mock('@/utility/misskey-api.js', () => ({
	misskeyApi: (...args: unknown[]) => h.api(...args),
	pendingApiRequestsCount: { value: 0 },
}));

import { Paginator } from '@/utility/paginator.js';

function items(n: number, from = 0) {
	return Array.from({ length: n }, (_, i) => ({ id: String(from + i) }));
}

/** The ids in the list (the paginator is built for a stand-in endpoint, so its items are untyped). */
function ids(p: Paginator<never>): string[] {
	return (p.items.value as unknown as { id: string }[]).map((x) => x.id);
}

/** A promise resolved (or rejected) from the outside. */
function deferred<T>() {
	let resolve!: (v: T) => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
	return { promise, resolve, reject };
}

/**
 * mk-go: 条件の変更や再読み込みで取り直したとき、古い取得の応答が後から届いても一覧に混ざらない (#4076)。
 *
 * init は世代 (#3132) を進めるが、以前は応答を待った後に世代を見ていなかった。古い取得の
 * 応答が遅れて届くと、新しい一覧に足され、読み込み中の表示も先に消えていた。fetchOlder にも
 * 世代の判定が無く、fetchNewer は世代の判定より前に 429 を記録していた。
 */
describe('Paginator の init の古い応答', () => {
	beforeEach(() => h.api.mockReset());

	test('前の init の応答が後から届いても、一覧に足さない', async () => {
		const first = deferred<unknown[]>();
		const second = deferred<unknown[]>();
		h.api.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
		const p = new Paginator('users/following' as never, { limit: 10 } as never);

		const a = p.init();
		const b = p.init();
		second.resolve(items(3, 100));
		await b;
		expect(ids(p)).toEqual(['100', '101', '102']);
		expect(p.fetching.value).toBe(false);

		first.resolve(items(5, 0));
		await a;
		expect(ids(p), '前の条件の応答が混ざった').toEqual(['100', '101', '102']);
	});

	test('前の init の応答が先に届いても、読み込み中の表示を消さない', async () => {
		const first = deferred<unknown[]>();
		const second = deferred<unknown[]>();
		h.api.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
		const p = new Paginator('users/following' as never, { limit: 10 } as never);

		const a = p.init();
		const b = p.init();
		first.resolve(items(5, 0));
		await a;
		expect(p.items.value, '前の条件の応答が先に出た').toEqual([]);
		expect(p.fetching.value, '後の取得を待っているのに読み込み中の表示が消えた').toBe(true);

		second.resolve(items(2, 100));
		await b;
		expect(ids(p)).toEqual(['100', '101']);
		expect(p.fetching.value).toBe(false);
	});

	test('前の init の失敗は、後の取得の状態を上書きしない', async () => {
		const first = deferred<unknown[]>();
		const second = deferred<unknown[]>();
		h.api.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
		const p = new Paginator('users/following' as never, { limit: 10 } as never);

		const a = p.init();
		const b = p.init();
		first.reject({ code: 'INTERNAL_ERROR' });
		await a;
		expect(p.error.value, '前の条件の失敗で失敗の表示になった').toBe(false);
		expect(p.fetching.value).toBe(true);

		second.resolve(items(1, 100));
		await b;
		expect(p.error.value).toBe(false);
		expect(ids(p)).toEqual(['100']);
	});

	test('前の init の 429 は、後の取得の状態を上書きしない', async () => {
		const first = deferred<unknown[]>();
		const second = deferred<unknown[]>();
		h.api.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
		const p = new Paginator('users/following' as never, { limit: 10 } as never);

		const a = p.init();
		const b = p.init();
		first.reject({ code: 'RATE_LIMIT_EXCEEDED' });
		await a;
		second.resolve([]);
		await b;
		expect(p.rateLimited.value, '古い取得の 429 がレート制限の印として残った').toBe(false);
		// 新しい一覧は空なので、続きは無い
		expect(p.canFetchOlder.value).toBe(false);
	});

	test('init の最中に届いた、古い一覧の fetchOlder の応答を足さない', async () => {
		h.api.mockResolvedValueOnce(items(10, 0));
		const p = new Paginator('users/following' as never, { limit: 10 } as never);
		await p.init();

		const older = deferred<unknown[]>();
		const fresh = deferred<unknown[]>();
		h.api.mockReturnValueOnce(older.promise).mockReturnValueOnce(fresh.promise);
		const o = p.fetchOlder();
		const b = p.reload();
		older.resolve(items(5, 50));
		await o;
		expect(ids(p), '古い一覧の続きが新しい一覧に入った').toEqual([]);

		fresh.resolve(items(2, 100));
		await b;
		expect(ids(p)).toEqual(['100', '101']);
	});

	test('init の後に届いた、古い一覧の fetchOlder の 429 を記録しない', async () => {
		h.api.mockResolvedValueOnce(items(10, 0));
		const p = new Paginator('users/following' as never, { limit: 10 } as never);
		await p.init();

		const older = deferred<unknown[]>();
		h.api.mockReturnValueOnce(older.promise).mockResolvedValueOnce(items(2, 100));
		const o = p.fetchOlder();
		await p.reload();
		older.reject({ code: 'RATE_LIMIT_EXCEEDED' });
		await o;
		expect(p.rateLimited.value, '古い一覧への 429 が新しい一覧の印になった').toBe(false);
		expect(ids(p)).toEqual(['100', '101']);
	});

	test('reload の後に届いた、古い世代の fetchNewer の 429 を記録しない', async () => {
		h.api.mockResolvedValueOnce(items(3, 0));
		const p = new Paginator('users/following' as never, { limit: 10 } as never);
		await p.init();

		const newer = deferred<unknown[]>();
		h.api.mockReturnValueOnce(newer.promise).mockResolvedValueOnce(items(2, 100));
		const n = p.fetchNewer({ toQueue: false });
		await p.reload();
		newer.reject({ code: 'RATE_LIMIT_EXCEEDED' });
		await n;
		expect(p.rateLimited.value, '古い世代への 429 が新しい一覧の印になった').toBe(false);
	});
});

