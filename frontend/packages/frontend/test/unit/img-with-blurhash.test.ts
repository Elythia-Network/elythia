/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// MkImgWithBlurhash の切り替えが、スタイルの再計算を強制しないことを固定する (#3935)。
// 以前は TransitionGroup で canvas と img を切り替えていて、画像のデコードのたびに
// getComputedStyle を数回読んでいた。アバターもこの部品なので、タイムラインの
// ノート 1 件ごとに起きていた。

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import * as sass from 'sass-embedded';
import { cleanup, render } from '@testing-library/vue';
import { nextTick } from 'vue';
import MkImgWithBlurhash from '@/components/MkImgWithBlurhash.vue';
import { prefer } from '@/preferences.js';

// rAF と Vue の遷移の後始末が一巡するのを待つ
async function settle(): Promise<void> {
	for (let i = 0; i < 5; i++) {
		await nextTick();
		await new Promise((r) => setTimeout(r, 20));
	}
}

// @testing-library/vue は既定で Transition と TransitionGroup をスタブに置き換える。
// スタブだと遷移が走らず、以前の実装でも getComputedStyle が呼ばれないので、本物を使う
function renderImg(props: Record<string, unknown>) {
	return render(MkImgWithBlurhash, { props, global: { stubs: { transition: false, 'transition-group': false } } });
}

function getImg(container: Element): HTMLImageElement {
	const img = container.querySelector('img');
	if (img == null) throw new Error('img not rendered');
	return img;
}

function getCanvas(container: Element): HTMLCanvasElement {
	const canvas = container.querySelector('canvas');
	if (canvas == null) throw new Error('canvas not rendered');
	return canvas;
}

async function loadImage(img: HTMLImageElement): Promise<void> {
	img.dispatchEvent(new Event('load'));
	await settle();
}

describe('MkImgWithBlurhash', () => {
	let computedStyle: ReturnType<typeof vi.spyOn>;
	let animation: unknown;

	beforeEach(() => {
		// テストの設定はモック (test/setup.unit.ts) で、ただのオブジェクト。
		// 以前の実装が getComputedStyle を読むのはアニメーションが有効なときなので、有効にして見る
		animation = prefer.s.animation;
		(prefer.s as Record<string, unknown>).animation = true;
		// happy-dom の decode() は実装により挙動が違うので、すぐ解決させる
		vi.spyOn(HTMLImageElement.prototype, 'decode').mockResolvedValue(undefined);
		computedStyle = vi.spyOn(window, 'getComputedStyle');
	});

	afterEach(() => {
		cleanup();
		(prefer.s as Record<string, unknown>).animation = animation;
		vi.restoreAllMocks();
	});

	test('switches from the placeholder to the image without reading computed style', async () => {
		// 切り替えの瞬間だけを見るため、デコードの完了はこちらで起こす
		let resolveDecode: () => void = () => {};
		vi.mocked(HTMLImageElement.prototype.decode).mockImplementation(() => new Promise<void>((r) => { resolveDecode = r; }));
		const { container } = renderImg({ src: 'https://example.com/a.png', width: 400, height: 300 });
		await settle();
		computedStyle.mockClear();

		getImg(container).dispatchEvent(new Event('load'));
		await settle();
		resolveDecode();
		await settle();

		expect(getCanvas(container).style.display).toBe('none');
		expect(computedStyle).not.toHaveBeenCalled();
	});

	test('the image is lazy and keeps its box while hidden', async () => {
		const { container } = renderImg({ src: 'https://example.com/a.png', width: 400, height: 300 });
		await settle();
		const img = getImg(container);

		expect(img.getAttribute('loading')).toBe('lazy');
		// display: none だと、lazy の画像は画面に近付いても読み込まれない
		expect(img.style.display).not.toBe('none');
		expect(getCanvas(container).style.display).not.toBe('none');
		const hiddenClass = [...img.classList];

		await loadImage(img);

		// 隠すためのクラスが外れる
		expect([...img.classList].length).toBeLessThan(hiddenClass.length);
	});

	test('keeps the placeholder until the image loads', async () => {
		const { container } = renderImg({ src: 'https://example.com/a.png' });
		await settle();
		expect(getCanvas(container).style.display).not.toBe('none');
		expect(HTMLImageElement.prototype.decode).not.toHaveBeenCalled();
	});

	test('takes the aspect ratio from the given size', async () => {
		const { container } = renderImg({ src: 'https://example.com/a.png', width: 400, height: 300 });
		await settle();
		const img = getImg(container);
		expect(img.getAttribute('width')).toBe('400');
		expect(img.getAttribute('height')).toBe('300');
	});

	test('keeps the placeholder while forceBlurhash is set', async () => {
		const { container } = renderImg({ src: 'https://example.com/a.png', forceBlurhash: true });
		await settle();
		await loadImage(getImg(container));
		expect(getCanvas(container).style.display).not.toBe('none');
	});

	test('does not switch on a stale load after src changed', async () => {
		let resolveDecode: () => void = () => {};
		vi.mocked(HTMLImageElement.prototype.decode).mockImplementationOnce(() => new Promise<void>((r) => { resolveDecode = r; }));
		const { container, rerender } = renderImg({ src: 'https://example.com/a.png' });
		await settle();
		getImg(container).dispatchEvent(new Event('load'));
		await rerender({ src: 'https://example.com/b.png' });
		resolveDecode();
		await settle();
		expect(getCanvas(container).style.display).not.toBe('none');
	});

	test('keeps the placeholder again when src goes away', async () => {
		const { container, rerender } = renderImg({ src: 'https://example.com/a.png' });
		await settle();
		await loadImage(getImg(container));
		expect(getCanvas(container).style.display).toBe('none');
		await rerender({ src: null });
		await settle();
		expect(getCanvas(container).style.display).not.toBe('none');
	});

	describe('with a transition (gallery)', () => {
		const transition = { duration: 500, leaveActiveClass: 'test-leave-active', leaveToClass: 'test-leave-to' };

		test('fades the image out when it gets hidden', async () => {
			const { container, rerender } = renderImg({ src: 'https://example.com/a.png', transition, forceBlurhash: false });
			await settle();
			// v-show で消すので、lazy だと隠したまま読み込まれない
			expect(getImg(container).getAttribute('loading')).toBe('eager');
			await loadImage(getImg(container));
			await rerender({ src: 'https://example.com/a.png', transition, forceBlurhash: true });
			await nextTick();
			await new Promise((r) => setTimeout(r, 50));
			// 退場の遷移の途中 (500ms) にいる
			expect(getImg(container).classList.contains('test-leave-active')).toBe(true);
			// 隠すクラスが付くと、遷移の途中で消えてフェードが見えない
			expect([...getImg(container).classList].some((c) => c.includes('imgHidden'))).toBe(false);
			expect(getCanvas(container).style.display).not.toBe('none');
		});

		test('fades the placeholder out when the image gets shown', async () => {
			const { container, rerender } = renderImg({ src: 'https://example.com/a.png', transition, forceBlurhash: true });
			await settle();
			await loadImage(getImg(container));
			await rerender({ src: 'https://example.com/a.png', transition, forceBlurhash: false });
			await nextTick();
			await new Promise((r) => setTimeout(r, 50));
			expect(getCanvas(container).classList.contains('test-leave-active')).toBe(true);
		});

		test('switches at once when animation is off', async () => {
			(prefer.s as Record<string, unknown>).animation = false;
			const { container, rerender } = renderImg({ src: 'https://example.com/a.png', transition, forceBlurhash: false });
			await settle();
			await loadImage(getImg(container));
			computedStyle.mockClear();
			await rerender({ src: 'https://example.com/a.png', transition, forceBlurhash: true });
			await settle();
			expect(getImg(container).classList.contains('test-leave-active')).toBe(false);
			expect(getCanvas(container).style.display).not.toBe('none');
			expect(computedStyle).not.toHaveBeenCalled();
		});
	});
});

describe('MkImgWithBlurhash styles', () => {
	// テストの環境では CSS Modules の style が注入されないので、SFC の style を実際にコンパイルして見る
	const source = readFileSync(join(__dirname, '../../src/components/MkImgWithBlurhash.vue'), 'utf8');
	const scss = source.match(/<style lang="scss" module>([\s\S]*?)<\/style>/)?.[1] ?? '';
	const css = sass.compileString(scss).css;
	const rule = css.match(/\.imgHidden\s*\{([^}]*)\}/)?.[1] ?? '';

	test('hides the image without removing its box', () => {
		expect(rule).toMatch(/visibility:\s*hidden/);
		// display: none だと、lazy の画像は画面に近付いても読み込まれない
		expect(rule).not.toMatch(/display:\s*none/);
	});

	test('overlays the hidden image on the placeholder instead of stacking it', () => {
		expect(rule).toMatch(/position:\s*absolute/);
		expect(rule).toMatch(/top:\s*0/);
		expect(rule).toMatch(/left:\s*0/);
	});
});
