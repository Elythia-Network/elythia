/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { NIGHT_METEOR_STYLE, NightMeteorRenderer, nextBurstDelay, startNightMeteorEffect } from '@/utility/night-meteor-effect.js';
import type { NightMeteorEnvironment } from '@/utility/night-meteor-effect.js';

/**
 * Elythia: 夜空の流れ星の描画と、始める・止める判断の配線 (#3530)。
 * 流れ星が飛んでいない間は canvas を置かず requestAnimationFrame も回さないこと、
 * 夜の境目・タブの表示・動きを減らす設定で始まり止まることを確かめる。
 */

let rafQueue: FrameRequestCallback[];
let rafSpy: ReturnType<typeof vi.spyOn>;
let reducedMotion: { matches: boolean; listeners: Set<() => void> };
let visibility: DocumentVisibilityState;
let colorStops: [number, string][];

function fakeContext(): CanvasRenderingContext2D {
	return {
		setTransform: vi.fn(),
		clearRect: vi.fn(),
		createLinearGradient: vi.fn(() => ({
			addColorStop: (offset: number, color: string) => colorStops.push([offset, color]),
		})),
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

/** The interval between the bursts while Math.random() returns 0.5. */
const BURST_MS = 25 * 1000;

beforeEach(() => {
	vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
	vi.setSystemTime(new Date(2026, 9, 13, 22, 0));
	vi.spyOn(Math, 'random').mockReturnValue(0.5);

	rafQueue = [];
	colorStops = [];
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
	test('puts the canvas and requests animation frames only while meteors are in flight', () => {
		const renderer = new NightMeteorRenderer();
		renderer.start();
		expect(renderer.running).toBe(true);

		// 最初の群れまでは canvas も置かず、何も描かない。
		vi.advanceTimersByTime(BURST_MS - 1000);
		expect(canvasCount()).toBe(0);
		expect(rafSpy).not.toHaveBeenCalled();

		// 群れが流れると canvas を置いて描き始める。
		vi.advanceTimersByTime(1000);
		expect(canvasCount()).toBe(1);
		expect(rafSpy).toHaveBeenCalledTimes(1);

		// 流れ終わったら、次を求めずに止まり、canvas も外す。
		const frames = runFrames();
		expect(frames).toBeGreaterThan(10);
		expect(rafQueue).toHaveLength(0);
		expect(canvasCount()).toBe(0);
		const calls = rafSpy.mock.calls.length;

		// 次の群れまでの間も、requestAnimationFrame は呼ばれない。
		vi.advanceTimersByTime(BURST_MS - 1000);
		expect(rafSpy).toHaveBeenCalledTimes(calls);
		expect(canvasCount()).toBe(0);

		// 次の群れでまた置く。
		vi.advanceTimersByTime(1000);
		expect(canvasCount()).toBe(1);

		renderer.stop();
	});

	test('keeps a single canvas, timer and resize listener when started twice', () => {
		const add = vi.spyOn(window, 'addEventListener');
		const renderer = new NightMeteorRenderer();
		renderer.start();
		renderer.start();
		expect(vi.getTimerCount()).toBe(1);

		vi.advanceTimersByTime(BURST_MS);
		expect(canvasCount()).toBe(1);
		expect(add.mock.calls.filter(([type]) => (type as string) === 'resize')).toHaveLength(1);

		renderer.stop();
	});

	test('removes the canvas, the resize listener and every timer on stop', () => {
		const add = vi.spyOn(window, 'addEventListener');
		const remove = vi.spyOn(window, 'removeEventListener');
		const renderer = new NightMeteorRenderer();
		renderer.start();
		vi.advanceTimersByTime(BURST_MS);
		expect(rafQueue.length).toBeGreaterThan(0);
		const resizeListener = add.mock.calls.find(([type]) => (type as string) === 'resize')?.[1];
		expect(resizeListener).toBeTypeOf('function');
		const canvas = window.document.querySelector('canvas') as HTMLCanvasElement;

		renderer.stop();
		expect(canvasCount()).toBe(0);
		expect(canvas.width).toBe(0);
		expect(canvas.height).toBe(0);
		expect(window.cancelAnimationFrame).toHaveBeenCalled();
		expect(vi.getTimerCount()).toBe(0);
		expect(renderer.running).toBe(false);
		expect(remove).toHaveBeenCalledWith('resize', resizeListener);
	});

	test('removes the resize listener when the burst ends', () => {
		const add = vi.spyOn(window, 'addEventListener');
		const remove = vi.spyOn(window, 'removeEventListener');
		const renderer = new NightMeteorRenderer();
		renderer.start();
		vi.advanceTimersByTime(BURST_MS);
		const resizeListener = add.mock.calls.find(([type]) => (type as string) === 'resize')?.[1];
		const canvas = window.document.querySelector('canvas') as HTMLCanvasElement;
		runFrames();
		expect(remove).toHaveBeenCalledWith('resize', resizeListener);
		expect(canvas.width).toBe(0);
		expect(canvas.height).toBe(0);
		renderer.stop();
	});

	test('sizes the canvas with the device pixel ratio and hides it from the input and screen readers', () => {
		vi.spyOn(window, 'devicePixelRatio', 'get').mockReturnValue(2);
		const renderer = new NightMeteorRenderer();
		renderer.start();
		vi.advanceTimersByTime(BURST_MS);
		const canvas = window.document.querySelector('canvas')!;
		expect(canvas.width).toBe(window.innerWidth * 2);
		expect(canvas.height).toBe(window.innerHeight * 2);
		expect(canvas.style.pointerEvents).toBe('none');
		expect(canvas.style.position).toBe('fixed');
		expect(canvas.getAttribute('aria-hidden')).toBe('true');

		// 群れごとに新しい canvas を置くので、前の群れと同じ画面の大きさでも描画先を合わせ直す。
		runFrames();
		vi.advanceTimersByTime(BURST_MS);
		const next = window.document.querySelector('canvas')!;
		expect(next).not.toBe(canvas);
		expect(next.width).toBe(window.innerWidth * 2);
		expect(next.height).toBe(window.innerHeight * 2);
		renderer.stop();
	});

	test('fades the tail to the same color, not to transparent black', () => {
		const renderer = new NightMeteorRenderer();
		renderer.start();
		vi.advanceTimersByTime(BURST_MS);
		runFrames();
		expect(colorStops.length).toBeGreaterThan(0);
		for (const [offset, color] of colorStops) {
			expect(color).toBe(offset === 0 ? NIGHT_METEOR_STYLE.color : `${NIGHT_METEOR_STYLE.color}00`);
		}
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
		expect(controller.showing).toBe(false);

		vi.advanceTimersByTime(59 * 1000);
		expect(controller.showing).toBe(false);
		vi.advanceTimersByTime(1000);
		expect(controller.showing).toBe(true);

		// 始まった後は、群れが来たときに canvas を置く。
		vi.advanceTimersByTime(BURST_MS);
		expect(canvasCount()).toBe(1);

		controller.stop();
	});

	test('stops when the night ends', () => {
		vi.setSystemTime(new Date(2026, 9, 14, 3, 59, 0));
		const controller = startNightMeteorEffect(getEnv);
		expect(controller.showing).toBe(true);

		vi.advanceTimersByTime(60 * 1000);
		expect(controller.showing).toBe(false);
		vi.advanceTimersByTime(BURST_MS * 2);
		expect(canvasCount()).toBe(0);

		controller.stop();
	});

	test('follows the theme on update()', () => {
		const controller = startNightMeteorEffect(getEnv);
		expect(controller.showing).toBe(true);

		env.darkMode = false;
		controller.update();
		expect(controller.showing).toBe(false);

		env.darkMode = true;
		controller.update();
		expect(controller.showing).toBe(true);

		controller.stop();
	});

	test('removes the canvas in flight when the conditions stop holding', () => {
		const controller = startNightMeteorEffect(getEnv);
		vi.advanceTimersByTime(BURST_MS);
		expect(canvasCount()).toBe(1);

		env.darkMode = false;
		controller.update();
		expect(canvasCount()).toBe(0);

		controller.stop();
	});

	test('does not start while the seasonal effect is showing', () => {
		env.seasonalEffectShowing = true;
		const controller = startNightMeteorEffect(getEnv);
		expect(controller.showing).toBe(false);
		controller.stop();
	});

	test('follows the page visibility', () => {
		const controller = startNightMeteorEffect(getEnv);
		expect(controller.showing).toBe(true);

		visibility = 'hidden';
		window.document.dispatchEvent(new Event('visibilitychange'));
		expect(controller.showing).toBe(false);

		visibility = 'visible';
		window.document.dispatchEvent(new Event('visibilitychange'));
		expect(controller.showing).toBe(true);

		controller.stop();
	});

	test('follows the reduced motion preference', () => {
		const controller = startNightMeteorEffect(getEnv);
		expect(controller.showing).toBe(true);

		reducedMotion.matches = true;
		for (const l of reducedMotion.listeners) l();
		expect(controller.showing).toBe(false);

		reducedMotion.matches = false;
		for (const l of reducedMotion.listeners) l();
		expect(controller.showing).toBe(true);

		controller.stop();
	});

	test('removes every listener and timer on stop', () => {
		const add = vi.spyOn(window.document, 'addEventListener');
		const remove = vi.spyOn(window.document, 'removeEventListener');
		const controller = startNightMeteorEffect(getEnv);
		const visibilityListener = add.mock.calls.find(([type]) => type === 'visibilitychange')?.[1];
		expect(visibilityListener).toBeTypeOf('function');

		controller.stop();
		expect(controller.showing).toBe(false);
		expect(vi.getTimerCount()).toBe(0);
		expect(reducedMotion.listeners.size).toBe(0);
		expect(remove).toHaveBeenCalledWith('visibilitychange', visibilityListener);

		window.document.dispatchEvent(new Event('visibilitychange'));
		controller.update();
		expect(controller.showing).toBe(false);
		expect(vi.getTimerCount()).toBe(0);
	});
});
