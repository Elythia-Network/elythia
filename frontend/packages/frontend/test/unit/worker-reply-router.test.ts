/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// worker の返事を id で配るクラスを固定する (#3935)。
// 以前は MkBlurhash が部品ごとに全 worker へリスナーを足していて、返事 1 通が
// 表示中の全部品に配られていた。待っている数によらずリスナーが 1 つであることを見る。

import { describe, expect, test, vi } from 'vitest';
import { WorkerReplyRouter } from '@/utility/worker-reply-router.js';

type Reply = { id: string; bitmap: string };

function fakeSource() {
	const listeners: ((ev: MessageEvent<unknown>) => unknown)[] = [];
	return {
		listeners,
		addListener(cb: (ev: MessageEvent<unknown>) => unknown) {
			listeners.push(cb);
		},
		emit(data: unknown) {
			for (const l of listeners) l.call({} as Worker, { data } as MessageEvent<unknown>);
		},
	};
}

describe('WorkerReplyRouter', () => {
	test('registers a single listener however many callers wait', () => {
		const src = fakeSource();
		const router = new WorkerReplyRouter<Reply>(src);
		for (let i = 0; i < 100; i++) router.set(`id${i}`, () => {});
		expect(src.listeners).toHaveLength(1);
		expect(router.size).toBe(100);
	});

	test('delivers a reply only to the caller with the same id', () => {
		const src = fakeSource();
		const router = new WorkerReplyRouter<Reply>(src);
		const a = vi.fn();
		const b = vi.fn();
		router.set('a', a);
		router.set('b', b);
		src.emit({ id: 'b', bitmap: 'B' });
		expect(a).not.toHaveBeenCalled();
		expect(b).toHaveBeenCalledExactlyOnceWith({ id: 'b', bitmap: 'B' });
	});

	test('stops delivering after delete', () => {
		const src = fakeSource();
		const router = new WorkerReplyRouter<Reply>(src);
		const a = vi.fn();
		router.set('a', a);
		router.delete('a');
		src.emit({ id: 'a', bitmap: 'A' });
		expect(a).not.toHaveBeenCalled();
		expect(router.size).toBe(0);
	});

	test('replaces the callback when the same id is set again', () => {
		const src = fakeSource();
		const router = new WorkerReplyRouter<Reply>(src);
		const first = vi.fn();
		const second = vi.fn();
		router.set('a', first);
		router.set('a', second);
		src.emit({ id: 'a', bitmap: 'A' });
		expect(first).not.toHaveBeenCalled();
		expect(second).toHaveBeenCalledOnce();
		expect(router.size).toBe(1);
	});

	test('ignores messages without a string id', () => {
		const src = fakeSource();
		const router = new WorkerReplyRouter<Reply>(src);
		const a = vi.fn();
		router.set('a', a);
		src.emit(null);
		src.emit({ bitmap: 'A' });
		src.emit({ id: 1, bitmap: 'A' });
		expect(a).not.toHaveBeenCalled();
	});
});
