/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it, vi } from 'vitest';
import { createPreloadableComponent, runWhenIdle } from '@/utility/preloadable-component.js';

// 投稿フォームを起動時の JS から外し、空き時間に先に読み込む (#3613)。
describe('createPreloadableComponent', () => {
	it('loads once and exposes the component synchronously afterwards', async () => {
		const component = { name: 'X' };
		const load = vi.fn(async () => ({ default: component }));
		const c = createPreloadableComponent(load);

		expect(c.loaded).toBeNull();
		const [a, b] = await Promise.all([c.preload(), c.preload()]);
		expect(a).toBe(component);
		expect(b).toBe(component);
		expect(c.loaded).toBe(component);
		await c.preload();
		expect(load).toHaveBeenCalledTimes(1);
	});

	it('can retry after a failed load', async () => {
		const component = { name: 'X' };
		const load = vi.fn()
			.mockRejectedValueOnce(new Error('chunk load failed'))
			.mockResolvedValueOnce({ default: component });
		const c = createPreloadableComponent(load);

		await expect(c.preload()).rejects.toThrow('chunk load failed');
		expect(c.loaded).toBeNull();
		await expect(c.preload()).resolves.toBe(component);
		expect(load).toHaveBeenCalledTimes(2);
	});
});

describe('runWhenIdle', () => {
	it('falls back to a timer without requestIdleCallback', async () => {
		const original = window.requestIdleCallback;
		// @ts-expect-error 対応していないブラウザの代わり
		window.requestIdleCallback = undefined;
		try {
			await new Promise<void>(resolve => runWhenIdle(resolve));
		} finally {
			window.requestIdleCallback = original;
		}
	});

	it('uses requestIdleCallback when present', () => {
		const original = window.requestIdleCallback;
		const ric = vi.fn((cb: IdleRequestCallback) => { cb({ didTimeout: false, timeRemaining: () => 10 }); return 1; });
		window.requestIdleCallback = ric;
		try {
			const fn = vi.fn();
			runWhenIdle(fn);
			expect(ric).toHaveBeenCalledTimes(1);
			expect(fn).toHaveBeenCalledTimes(1);
		} finally {
			window.requestIdleCallback = original;
		}
	});
});

// os.ts が投稿フォームを静的に import し直すと、起動時の JS に一式が戻る。
describe('os.ts post form loading', () => {
	const source = readFileSync(join(__dirname, '../../src/os.ts'), 'utf8');

	it('does not import MkPostFormDialog statically', () => {
		expect(source).not.toMatch(/^import\s+MkPostFormDialog\s+from/m);
		expect(source).toContain("import('@/components/MkPostFormDialog.vue')");
	});
});
