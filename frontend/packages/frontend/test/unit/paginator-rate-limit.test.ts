/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, beforeEach, vi } from 'vitest';

const h = vi.hoisted(() => ({ api: vi.fn() }));
vi.mock('@/utility/misskey-api.js', () => ({
	misskeyApi: (...args: unknown[]) => h.api(...args),
	pendingApiRequestsCount: { value: 0 },
}));

import { Paginator } from '@/utility/paginator.js';

const RATE_LIMIT = { code: 'RATE_LIMIT_EXCEEDED' };

function items(n: number, from = 0) {
	return Array.from({ length: n }, (_, i) => ({ id: String(from + i) }));
}

async function loaded() {
	h.api.mockReset();
	h.api.mockResolvedValueOnce(items(15));
	const p = new Paginator('users/following' as never, { limit: 15 } as never);
	await p.init();
	return p;
}

/**
 * mk-go: 429 を受けたら追い読みの自動発火を止める (#2955)。
 *
 * **文字列ゲートでは守れない。** 当初は Go 側で `paginator.ts` の字面を見て
 * いたが、敵対的レビューで **11 変異中 7 件を素通り**することを実測された。
 * 特に「`canFetch*` を落とさず印だけ立てる」「向きを取り違える」という、
 * この修正の本体そのものを壊す変異が通っていた。振る舞いで固定する。
 */
describe('Paginator のレート制限時の停止', () => {
	beforeEach(() => h.api.mockReset());

	test('429 を受けたら、その向きの追い読みを止めて二度と撃たない', async () => {
		const p = await loaded();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchOlder();

		expect(p.canFetchOlder.value).toBe(false);
		expect(p.rateLimited.value).toBe(true);
		expect(p.rateLimitedDirection.value).toBe('older');

		const before = h.api.mock.calls.length;
		await p.fetchOlder();
		expect(h.api.mock.calls.length).toBe(before);
	});

	test('**他のエラーでは止めない。** 一時的なもので、再試行すれば直る', async () => {
		const p = await loaded();
		h.api.mockRejectedValueOnce({ code: 'INTERNAL_ERROR' });
		await p.fetchOlder();
		expect(p.rateLimited.value).toBe(false);
		expect(p.canFetchOlder.value).toBe(true);
	});

	test('**アイテムが届いても停止を解除しない。** trim が復活させると streaming で自走が戻る', async () => {
		// **MAX_ITEMS (30) を超えて積む。** `trim` の復活は
		// `items.length >= MAX_ITEMS` が条件なので、それ未満だと分岐に到達せず
		// **ガードを外しても落ちない** (当初これで空振りしていた)。
		h.api.mockReset();
		h.api.mockResolvedValueOnce(items(30));
		const p = new Paginator('users/following' as never, { limit: 30 } as never);
		await p.init();
		expect(p.items.value.length).toBeGreaterThanOrEqual(30);

		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchOlder();
		expect(p.canFetchOlder.value).toBe(false);

		p.prepend({ id: 'new-1' } as never);
		expect(p.canFetchOlder.value).toBe(false);
		p.unshiftItems([{ id: 'new-2' }] as never);
		expect(p.canFetchOlder.value).toBe(false);
	});

	test('**成功したら解除する。** 残すと制限中でもないのに制限中と表示する', async () => {
		const p = await loaded();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchOlder();
		expect(p.rateLimited.value).toBe(true);

		h.api.mockResolvedValueOnce(items(15));
		await p.reload();
		expect(p.rateLimited.value).toBe(false);
		expect(p.rateLimitedDirection.value).toBe(null);
	});

	test('**初回の取得の 429 も区別する。** 汎用エラーに潰すと「何かがおかしいようです」になる', async () => {
		h.api.mockReset();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		const p = new Paginator('users/following' as never, { limit: 15 } as never);
		await p.init();
		expect(p.rateLimited.value).toBe(true);
	});

	test('**レート制限中は fetchNewer も撃たない。** polling は canFetchNewer を見ない', async () => {
		const p = await loaded();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchOlder();

		const before = h.api.mock.calls.length;
		await p.fetchNewer({ toQueue: false });
		expect(h.api.mock.calls.length).toBe(before);
	});

	test('**再試行は止めた向きだけを戻して、その向きを撃つ。** 無条件に戻すと別の向きが復活する', async () => {
		const p = await loaded();
		p.canFetchNewer.value = false; // 終端に達している側
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchOlder();

		h.api.mockResolvedValueOnce(items(5, 100));
		await p.retryAfterRateLimit();

		expect(p.canFetchOlder.value).toBe(true);
		// 止めていない向きは戻さない。
		expect(p.canFetchNewer.value).toBe(false);
		expect(p.rateLimited.value).toBe(false);
		// 実際に読み直していること。
		expect(h.api.mock.calls.length).toBeGreaterThan(2);
	});

	test('**newer で止まったら newer を撃つ。** older を撃つと止まった側が戻らない', async () => {
		const p = await loaded();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchNewer({ toQueue: false });
		expect(p.rateLimitedDirection.value).toBe('newer');

		h.api.mockResolvedValueOnce(items(3, 200));
		await p.retryAfterRateLimit();
		expect(p.canFetchNewer.value).toBe(true);
		// **どちらを撃ったかを引数で見る (レビュー 2 周目 M-4)。** `canFetchNewer`
		// は `retryAfterRateLimit` の**前半**が立てる値なので、後半 (どちらを
		// 撃つか) を `fetchOlder` 固定に変えても通ってしまう。newer の取得は
		// `sinceId` を、older は `untilId` を載せるので、そこで区別する。
		const lastArgs = h.api.mock.calls.at(-1)?.[1] as Record<string, unknown>;
		expect(Object.keys(lastArgs)).toContain('sinceId');
		expect(Object.keys(lastArgs)).not.toContain('untilId');
	});

	test('**older で止まったら older を撃つ。**', async () => {
		const p = await loaded();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchOlder();

		h.api.mockResolvedValueOnce(items(3, 300));
		await p.retryAfterRateLimit();
		const lastArgs = h.api.mock.calls.at(-1)?.[1] as Record<string, unknown>;
		expect(Object.keys(lastArgs)).toContain('untilId');
	});

	test('**初回取得で止まったら init を撃つ。** fetchOlder は items が空だと早期 return する', async () => {
		h.api.mockReset();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		const p = new Paginator('users/following' as never, { limit: 15 } as never);
		await p.init();
		expect(p.rateLimited.value).toBe(true);
		expect(p.items.value.length).toBe(0);

		const before = h.api.mock.calls.length;
		h.api.mockResolvedValueOnce(items(5));
		await p.retryAfterRateLimit();
		// **実際に読み直していること。** fetchOlder を撃つと無反応で印だけ消える。
		expect(h.api.mock.calls.length).toBeGreaterThan(before);
		expect(p.items.value.length).toBe(5);
		expect(p.rateLimited.value).toBe(false);
	});

	test('**別方向の in-flight 成功が印を消さない (レビュー 2 周目 M-1)。** 消すと「これで全部」に見える', async () => {
		const p = await loaded();

		// **先に in-flight にするのが要点。** `fetchNewer` の冒頭ガードは
		// 呼び出し時に見るので、429 の**後**に呼ぶ形では到達しない。poll が
		// 走っている最中に別方向が 429 になる、という順序を再現する。
		let resolveNewer!: (v: unknown) => void;
		h.api.mockImplementationOnce(() => new Promise(r => { resolveNewer = r; }));
		const inFlight = p.fetchNewer({ toQueue: false });

		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchOlder();
		expect(p.rateLimited.value).toBe(true);

		resolveNewer(items(2, 400));
		await inFlight;
		// 印が消えると notice も「もっと見る」も無い =「これで全部」に見える。
		expect(p.rateLimited.value).toBe(true);
	});

	test('**解除の経路は再試行と再読み込みだけ (レビュー 3 周目 H-2)。** 成功で解除すると in-flight の競合で印だけ消える', async () => {
		const p = await loaded();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchNewer({ toQueue: false });
		expect(p.rateLimited.value).toBe(true);

		// 429 の後はどちらの向きもガードで止まるので、成功する取得は来ない。
		const before = h.api.mock.calls.length;
		await p.fetchOlder();
		await p.fetchNewer({ toQueue: false });
		expect(h.api.mock.calls.length).toBe(before);
		expect(p.rateLimited.value).toBe(true);

		// 解除できるのは再試行から。
		h.api.mockResolvedValueOnce(items(5, 500));
		await p.retryAfterRateLimit();
		expect(p.rateLimited.value).toBe(false);
	});

	test('**冷却は Paginator が持つ (レビュー 3 周目 H-3)。** コンポーネントに置くと枝の移動で消える', async () => {
		const p = await loaded();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchOlder();
		expect(p.canRetryAfterRateLimit.value).toBe(true);

		h.api.mockResolvedValueOnce(items(3, 600));
		await p.retryAfterRateLimit();
		// 押した直後は冷却中。
		expect(p.canRetryAfterRateLimit.value).toBe(false);

		// 冷却中は撃たない。
		const before = h.api.mock.calls.length;
		await p.retryAfterRateLimit();
		expect(h.api.mock.calls.length).toBe(before);
	});

	test('**429 以外で init が失敗したら印を落とす (レビュー 3 周目 H-1)。** 残すと嘘の診断になる', async () => {
		h.api.mockReset();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		const p = new Paginator('users/following' as never, { limit: 15 } as never);
		await p.init();
		expect(p.rateLimited.value).toBe(true);

		// 次はネットワーク断。レート制限ではないので印は残さない。
		h.api.mockRejectedValueOnce(new TypeError('Failed to fetch'));
		await p.init();
		expect(p.rateLimited.value).toBe(false);
		expect(p.error.value).toBe(true);
	});

	test('**newer で止めたら older の自動発火も止まる (レビュー 3 周目 M-3)。**', async () => {
		const p = await loaded();
		h.api.mockRejectedValueOnce(RATE_LIMIT);
		await p.fetchNewer({ toQueue: false });

		const before = h.api.mock.calls.length;
		await p.fetchOlder();
		expect(h.api.mock.calls.length).toBe(before);
	});
});
