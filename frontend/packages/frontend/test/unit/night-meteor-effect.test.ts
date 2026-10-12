/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { NightMeteorRenderer, nextBurstDelay, startNightMeteorEffect } from '@/utility/night-meteor-effect.js';
import type { NightMeteorEnvironment } from '@/utility/night-meteor-effect.js';

/**
 * Elythia: 夜空の流れ星の描画と、始める・止める判断の配線 (#3530)。
 * 流れ星が飛んでいない間は requestAnimationFrame を回さないこと、
 * 夜の境目・タブの表示・動きを減らす設定で始まり止まることを確かめる。
 */

let rafQueue: FrameRequestCallback[];
let rafSpy: ReturnType<typeof vi.spyOn>;
let reducedMotion: { matches: boolean; listeners: Set<() => void> };
let visibility: DocumentVisibilityState;

function fakeContext(): CanvasRenderingContext2D {
	return {
		setTransform: vi.fn(),
		clearRect: vi.fn(),
		createLinearGradient: vi.fn(() => ({ addColorStop: vi.fn() })),
		beginPath: vi.fn(),
		moveTo: vi.fn(),
		lineTo: vi.fn(),
		stroke: vi.fn(),
		arc: vi.fn(),
		fill: vi.fn(),
	} as unknown as CanvasRenderingContext2D;
}

/** Runs the queued animation frames until none is requested. Returns how many ran. */
function runFrames(limit = 2000): number {
	let t = 1000;
	let count = 0;
	while (rafQueue.length > 0 && count < limit) {
		const cb = rafQueue.shift()!;
		t += 16;
		cb(t);
		count++;
	}
	return count;
}

function canvasCount(): number {
	return window.document.querySelectorAll('canvas').length;
}

beforeEach(() => {
	vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
	vi.setSystemTime(new Date(2026, 9, 13, 22, 0));
	vi.spyOn(Math, 'random').mockReturnValue(0.5);

	rafQueue = [];
	let rafId = 0;
	rafSpy = vi.spyOn(window, 'requestAnimationFrame').mockImplementation((cb) => {
		rafQueue.push(cb);
		return ++rafId;
	});
	vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(() => {
		rafQueue = [];
	});
	vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockImplementation(fakeContext as never);

	reducedMotion = { matches: false, listeners: new Set() };
	vi.spyOn(window, 'matchMedia').mockImplementation(() => ({
		get matches() { return reducedMotion.matches; },
		addEventListener: (_: string, l: () => void) => reducedMotion.listeners.add(l),
		removeEventListener: (_: string, l: () => void) => reducedMotion.listeners.delete(l),
	}) as unknown as MediaQueryList);

	visibility = 'visible';
	Object.defineProperty(window.document, 'visibilityState', {
		configurable: true,
		get: () => visibility,
	});
});

afterEach(() => {
	vi.useRealTimers();
	vi.restoreAllMocks();
	delete (window.document as { visibilityState?: unknown }).visibilityState;
	for (const c of window.document.querySelectorAll('canvas')) c.remove();
});

describe('nextBurstDelay', () => {
	test('stays within 0.6 to 1.4 times of 25 seconds', () => {
		expect(nextBurstDelay(() => 0)).toBe(15 * 1000);
		expect(nextBurstDelay(() => 0.5)).toBe(25 * 1000);
		expect(nextBurstDelay(() => 1)).toBe(35 * 1000);
	});
});

describe('NightMeteorRenderer', () => {
	test('does not request animation frames while no meteor is in flight', () => {
		const renderer = new NightMeteorRenderer();
		renderer.start();
		expect(canvasCount()).toBe(1);

		// 最初の群れまでは何も描かない。
		vi.advanceTimersByTime(24 * 1000);
		expect(rafSpy).not.toHaveBeenCalled();

		// 群れが流れると描き始める。
		vi.advanceTimersByTime(1000);
		expect(rafSpy).toHaveBeenCalledTimes(1);

		// 流れ終わったら、次を求めずに止まる。
		const frames = runFrames();
		expect(frames).toBeGreaterThan(10);
		expect(rafQueue).toHaveLength(0);
		const calls = rafSpy.mock.calls.length;

		// 次の群れまでの間も、requestAnimationFrame は呼ばれない。
		vi.advanceTimersByTime(24 * 1000);
		expect(rafSpy).toHaveBeenCalledTimes(calls);

		renderer.stop();
	});

	test('removes the canvas and every timer on stop', () => {
		const renderer = new NightMeteorRenderer();
		renderer.start();
		vi.advanceTimersByTime(25 * 1000);
		expect(rafQueue.length).toBeGreaterThan(0);

		renderer.stop();
		expect(canvasCount()).toBe(0);
		expect(window.cancelAnimationFrame).toHaveBeenCalled();
		expect(vi.getTimerCount()).toBe(0);
		expect(renderer.running).toBe(false);
	});

	test('sizes the canvas with the device pixel ratio again after a restart', () => {
		vi.spyOn(window, 'devicePixelRatio', 'get').mockReturnValue(2);
		const renderer = new NightMeteorRenderer();
		renderer.start();
		renderer.stop();
		renderer.start();
		const canvas = window.document.querySelector('canvas')!;
		expect(canvas.width).toBe(window.innerWidth * 2);
		expect(canvas.height).toBe(window.innerHeight * 2);
		expect(canvas.style.pointerEvents).toBe('none');
		renderer.stop();
	});
});

describe('startNightMeteorEffect', () => {
	const env: NightMeteorEnvironment = { darkMode: true, seasonalEffectShowing: false };
	const getEnv = () => env;

	afterEach(() => {
		env.darkMode = true;
		env.seasonalEffectShowing = false;
	});

	test('starts when the night begins', () => {
		vi.setSystemTime(new Date(2026, 9, 13, 19, 59, 0));
		const controller = startNightMeteorEffect(getEnv);
		expect(canvasCount()).toBe(0);

		vi.advanceTimersByTime(59 * 1000);
		expect(canvasCount()).toBe(0);
		vi.advanceTimersByTime(1000);
		expect(canvasCount()).toBe(1);

		controller.stop();
	});

	test('stops when the night ends', () => {
		vi.setSystemTime(new Date(2026, 9, 14, 3, 59, 0));
		const controller = startNightMeteorEffect(getEnv);
		expect(canvasCount()).toBe(1);

		vi.advanceTimersByTime(60 * 1000);
		expect(canvasCount()).toBe(0);

		controller.stop();
	});

	test('follows the theme on update()', () => {
		const controller = startNightMeteorEffect(getEnv);
		expect(canvasCount()).toBe(1);

		env.darkMode = false;
		controller.update();
		expect(canvasCount()).toBe(0);

		env.darkMode = true;
		controller.update();
		expect(canvasCount()).toBe(1);

		controller.stop();
	});

	test('does not start while the seasonal effect is showing', () => {
		env.seasonalEffectShowing = true;
		const controller = startNightMeteorEffect(getEnv);
		expect(canvasCount()).toBe(0);
		controller.stop();
	});

	test('follows the page visibility', () => {
		const controller = startNightMeteorEffect(getEnv);
		expect(canvasCount()).toBe(1);

		visibility = 'hidden';
		window.document.dispatchEvent(new Event('visibilitychange'));
		expect(canvasCount()).toBe(0);

		visibility = 'visible';
		window.document.dispatchEvent(new Event('visibilitychange'));
		expect(canvasCount()).toBe(1);

		controller.stop();
	});

	test('follows the reduced motion preference', () => {
		const controller = startNightMeteorEffect(getEnv);
		expect(canvasCount()).toBe(1);

		reducedMotion.matches = true;
		for (const l of reducedMotion.listeners) l();
		expect(canvasCount()).toBe(0);

		reducedMotion.matches = false;
		for (const l of reducedMotion.listeners) l();
		expect(canvasCount()).toBe(1);

		controller.stop();
	});

	test('removes every listener and timer on stop', () => {
		const add = vi.spyOn(window.document, 'addEventListener');
		const remove = vi.spyOn(window.document, 'removeEventListener');
		const controller = startNightMeteorEffect(getEnv);
		const visibilityListener = add.mock.calls.find(([type]) => type === 'visibilitychange')?.[1];
		expect(visibilityListener).toBeTypeOf('function');

		controller.stop();
		expect(canvasCount()).toBe(0);
		expect(vi.getTimerCount()).toBe(0);
		expect(reducedMotion.listeners.size).toBe(0);
		expect(remove).toHaveBeenCalledWith('visibilitychange', visibilityListener);

		window.document.dispatchEvent(new Event('visibilitychange'));
		controller.update();
		expect(canvasCount()).toBe(0);
	});
});
