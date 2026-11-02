/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 中身の組み立てを数フレームに分けるスケジューラを固定する (#3937)。

import { afterEach, describe, test, expect, vi } from 'vitest';
import { createMountScheduler, deferOlderMounts, nextFrame } from '@/utility/mount-scheduler.js';

// フレームの区切りをテストから進める
function manualFrames() {
	const waiting: (() => void)[] = [];
	return {
		waitFrame: () => new Promise<void>(r => { waiting.push(r); }),
		async next() {
			waiting.shift()?.();
			for (let i = 0; i < 5; i++) await Promise.resolve();
		},
		get waiting() {
			return waiting.length;
		},
	};
}

describe('createMountScheduler', () => {
	test('starts at most perFrame requests on each frame, in order', async () => {
		const frames = manualFrames();
		const s = createMountScheduler(2, frames.waitFrame);
		const started: number[] = [];
		for (let i = 0; i < 5; i++) s.request(() => started.push(i));

		// 頼んだその場では始めない
		expect(started).toEqual([]);
		await frames.next();
		expect(started).toEqual([0, 1]);
		await frames.next();
		expect(started).toEqual([0, 1, 2, 3]);
		await frames.next();
		expect(started).toEqual([0, 1, 2, 3, 4]);
		expect(s.pending).toBe(0);
		// 空になったらフレームを待ち続けない
		expect(frames.waiting).toBe(0);
	});

	test('does not start a cancelled request', async () => {
		const frames = manualFrames();
		const s = createMountScheduler(2, frames.waitFrame);
		const a = vi.fn();
		const b = vi.fn();
		const cancel = s.request(a);
		s.request(b);
		cancel();
		expect(s.pending).toBe(1);

		await frames.next();
		expect(a).not.toHaveBeenCalled();
		expect(b).toHaveBeenCalledOnce();
	});

	test('runs again for requests made after the queue drained', async () => {
		const frames = manualFrames();
		const s = createMountScheduler(1, frames.waitFrame);
		const a = vi.fn();
		const b = vi.fn();
		s.request(a);
		await frames.next();
		expect(a).toHaveBeenCalledOnce();

		s.request(b);
		await frames.next();
		expect(b).toHaveBeenCalledOnce();
	});

	test('runs the same function as many times as it was requested', async () => {
		const frames = manualFrames();
		const s = createMountScheduler(4, frames.waitFrame);
		const a = vi.fn();
		s.request(a);
		s.request(a);
		await frames.next();
		expect(a).toHaveBeenCalledTimes(2);
	});
});

describe('deferOlderMounts', () => {
	test('defers only items older than the list while the wrapped fetch runs', async () => {
		let resolve!: () => void;
		const m = deferOlderMounts(() => new Promise<void>(r => { resolve = r; }), () => ['n050', 'n040', 'n045']);
		expect(m.shouldDefer('n010')).toBe(false);

		const done = m.fetchOlder();
		expect(m.shouldDefer('n039')).toBe(true);
		// 境は手持ちの最古 (並び順によらない)
		expect(m.shouldDefer('n040')).toBe(false);
		// 応答を待つ間に届いた新着
		expect(m.shouldDefer('n060')).toBe(false);
		resolve();
		await done;
		expect(m.shouldDefer('n039')).toBe(false);
	});

	test('stops deferring even when the fetch fails, and defers again on the next fetch', async () => {
		let fail = true;
		let seen: boolean | null = null;
		const m = deferOlderMounts(async () => {
			if (fail) throw new Error('network');
			seen = m.shouldDefer('n001');
		}, () => ['n050']);
		await expect(m.fetchOlder()).rejects.toThrow('network');
		expect(m.shouldDefer('n001')).toBe(false);

		// 失敗の後も、次の呼び出しで境を作る (実行中の印が残っていない)
		fail = false;
		await m.fetchOlder();
		expect(seen).toBe(true);
	});

	test('keeps the boundary when called again while running', async () => {
		let resolve!: () => void;
		let calls = 0;
		const m = deferOlderMounts(() => {
			calls++;
			// paginator は実行中の 2 回目をすぐに返す
			return calls === 1 ? new Promise<void>(r => { resolve = r; }) : Promise.resolve();
		}, () => ['n050']);
		const first = m.fetchOlder();
		await m.fetchOlder();
		expect(calls).toBe(2);
		expect(m.shouldDefer('n001')).toBe(true);
		resolve();
		await first;
		expect(m.shouldDefer('n001')).toBe(false);
	});

	test('defers nothing when the list is empty', async () => {
		const m = deferOlderMounts(async () => {
			expect(m.shouldDefer('n001')).toBe(false);
		}, () => []);
		await m.fetchOlder();
	});
});

describe('nextFrame', () => {
	let visibility: DocumentVisibilityState = 'visible';
	Object.defineProperty(window.document, 'visibilityState', { configurable: true, get: () => visibility });

	afterEach(() => {
		visibility = 'visible';
		vi.restoreAllMocks();
	});

	test('waits for an animation frame without a timer in a visible tab', async () => {
		const timer = vi.spyOn(window, 'setTimeout');
		const raf = vi.spyOn(window, 'requestAnimationFrame');
		await nextFrame();
		expect(raf).toHaveBeenCalledOnce();
		// タイマーも張ると、重いフレームの後にフレームの外で組み立ててしまう
		expect(timer).not.toHaveBeenCalled();
	});

	test('goes on as soon as the tab becomes visible', async () => {
		visibility = 'hidden';
		let resolved = false;
		const done = nextFrame(60_000).then(() => {
			resolved = true;
		});
		visibility = 'visible';
		window.document.dispatchEvent(new Event('visibilitychange'));
		await done;
		expect(resolved).toBe(true);
	});

	test('falls back to a timer in a hidden tab', async () => {
		visibility = 'hidden';
		const raf = vi.spyOn(window, 'requestAnimationFrame');
		await nextFrame(1);
		// rAF は裏のタブでは呼ばれないので、頼まない
		expect(raf).not.toHaveBeenCalled();
	});

	test.each([
		['the timer', () => {}],
		['visibilitychange', () => window.document.dispatchEvent(new Event('visibilitychange'))],
	])('cleans up the listener and the timer when resolved by %s', async (_, trigger) => {
		visibility = 'hidden';
		const removed = vi.spyOn(window.document, 'removeEventListener');
		const cleared = vi.spyOn(window, 'clearTimeout');
		const done = nextFrame(1);
		trigger();
		await done;
		// 裏のタブでは毎回張るので、残すとフレームごとに増えていく
		expect(removed).toHaveBeenCalledWith('visibilitychange', expect.any(Function));
		expect(cleared).toHaveBeenCalled();
	});
});
