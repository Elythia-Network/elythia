/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { createSequentialPoller } from '@/utility/sequential-poller.js';

type Pending = { resolve: (v: boolean) => void; isCurrent: () => boolean };

/**
 * Elythia: 管理画面のリストアの進み具合の読み直し (#3463 (6c))。前の要求が終わる
 * までは次を出さず、止めた後に届いた応答は捨てる。
 */
describe('createSequentialPoller', () => {
	let pending: Pending[];
	const tick = (isCurrent: () => boolean) => new Promise<boolean>(resolve => { pending.push({ resolve, isCurrent }); });

	beforeEach(() => {
		vi.useFakeTimers();
		pending = [];
	});
	afterEach(() => {
		vi.useRealTimers();
	});

	test('does not send the next request while one is in flight', async () => {
		const p = createSequentialPoller(tick, 2000, window);
		p.start();
		expect(pending).toHaveLength(1);
		await vi.advanceTimersByTimeAsync(10000);
		expect(pending).toHaveLength(1);
		pending[0].resolve(true);
		await vi.advanceTimersByTimeAsync(1999);
		expect(pending).toHaveLength(1);
		await vi.advanceTimersByTimeAsync(1);
		expect(pending).toHaveLength(2);
		p.stop();
	});

	test('stops when tick returns false', async () => {
		const p = createSequentialPoller(tick, 2000, window);
		p.start();
		pending[0].resolve(false);
		await vi.advanceTimersByTimeAsync(10000);
		expect(pending).toHaveLength(1);
	});

	test('keeps polling after tick throws', async () => {
		const failing = vi.fn(async () => { throw new Error('down'); });
		const p = createSequentialPoller(failing, 2000, window);
		p.start();
		await vi.advanceTimersByTimeAsync(2000);
		expect(failing).toHaveBeenCalledTimes(2);
		p.stop();
	});

	test('drops what was in flight when stopped or restarted', async () => {
		const p = createSequentialPoller(tick, 2000, window);
		p.start();
		const first = pending[0];
		p.start();
		expect(pending).toHaveLength(2);
		expect(first.isCurrent()).toBe(false);
		expect(pending[1].isCurrent()).toBe(true);
		first.resolve(true);
		await vi.advanceTimersByTimeAsync(10000);
		expect(pending).toHaveLength(2);
		p.stop();
		expect(pending[1].isCurrent()).toBe(false);
		pending[1].resolve(true);
		await vi.advanceTimersByTimeAsync(10000);
		expect(pending).toHaveLength(2);
	});

	test('a stop cancels the scheduled request', async () => {
		const p = createSequentialPoller(tick, 2000, window);
		p.start();
		pending[0].resolve(true);
		await vi.advanceTimersByTimeAsync(1000);
		p.stop();
		await vi.advanceTimersByTimeAsync(10000);
		expect(pending).toHaveLength(1);
	});
});
