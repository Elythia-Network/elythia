/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 古いものを読み足したときに、中身を後から組み立てる部品を固定する (#3937)。

import { afterEach, describe, expect, test, vi } from 'vitest';
import { cleanup, render } from '@testing-library/vue';
import { defineComponent, h, inject, nextTick, ref, TransitionGroup } from 'vue';

const sched = vi.hoisted(() => ({ starts: [] as (() => void)[], cancelled: 0 }));
vi.mock('@/utility/mount-scheduler.js', async (importOriginal) => ({
	...await importOriginal<typeof import('@/utility/mount-scheduler.js')>(),
	mountScheduler: {
		request(start: () => void) {
			sched.starts.push(start);
			return () => {
				sched.cancelled++;
			};
		},
		pending: 0,
	},
}));

import MkDeferredMount from '@/components/MkDeferredMount.vue';
import { deferOlderMounts } from '@/utility/mount-scheduler.js';
import { FAR_ITEMS, ITEM_DISPOSE, ITEM_STATE } from '@/utility/far-items.js';
import type { FarItemCallback, FarItems } from '@/utility/far-items.js';

async function grantAll(): Promise<void> {
	for (const start of sched.starts.splice(0)) start();
	await nextTick();
}

const Content = defineComponent({
	props: { label: String },
	setup: (props) => () => h('article', props.label),
});

afterEach(() => {
	cleanup();
	sched.starts.length = 0;
	sched.cancelled = 0;
});

describe('MkDeferredMount', () => {
	test('renders the content at once and passes attributes to it without defer', () => {
		const { container } = render(() => h(MkDeferredMount, { class: 'note', 'data-scroll-anchor': 'a' }, () => h(Content, { label: 'A' })));
		const article = container.querySelector('article');
		expect(article?.textContent).toBe('A');
		// 包まない (以前と同じ DOM)
		expect(article?.parentElement).toBe(container);
		expect(article?.classList.contains('note')).toBe(true);
		expect(article?.getAttribute('data-scroll-anchor')).toBe('a');
		expect(sched.starts).toHaveLength(0);
	});

	test('renders a placeholder first and the content when given a turn with defer', async () => {
		const { container } = render(() => h(MkDeferredMount, { defer: true, class: 'note', 'data-scroll-anchor': 'a' }, () => h(Content, { label: 'A' })));
		const wrapper = container.firstElementChild as HTMLElement;
		expect(container.querySelector('article')).toBeNull();
		expect(wrapper.style.height).toBe('160px');
		expect(wrapper.classList.contains('note')).toBe(true);
		expect(wrapper.getAttribute('data-scroll-anchor')).toBe('a');

		await grantAll();
		expect(wrapper.querySelector('article')?.textContent).toBe('A');
		expect(wrapper.style.height).toBe('');
		// 根の要素を替えない
		expect(container.firstElementChild).toBe(wrapper);
	});

	test('cancels its turn when unmounted before it comes', () => {
		const { unmount } = render(() => h(MkDeferredMount, { defer: true }, () => h(Content)));
		unmount();
		expect(sched.cancelled).toBe(1);
	});

	test('does not run transitions when the content comes in a TransitionGroup', async () => {
		const { container } = render(() => h(TransitionGroup, { tag: 'div', name: 't' }, () => [
			h(MkDeferredMount, { key: 'a', defer: true }, () => h(Content, { label: 'A' })),
		]), { global: { stubs: { 'transition-group': false } } });
		// 退場の遷移はフレームをまたいで終わるので、替えた直後を見る
		await grantAll();

		const group = container.firstElementChild as HTMLElement;
		// 根を替えると、枠に退場の、中身に入場の遷移が掛かる (ブラウザでは強制レイアウトと
		// 0.7 秒のアニメーション。CSS の無いこの環境では、退場はすぐ終わり入場のクラスが残る)
		expect(group.children).toHaveLength(1);
		expect(group.querySelector('[class*="t-enter"], [class*="t-leave"]')).toBeNull();
		expect(group.querySelector('article')?.textContent).toBe('A');
	});
});

describe('deferOlderMounts with MkDeferredMount', () => {
	// タイムラインと同じつなぎ方をした一覧
	function renderList() {
		const items = ref(['n5', 'n4']);
		const m = deferOlderMounts(async () => {
			items.value.push('n3', 'n2');
		}, () => items.value);
		const List = defineComponent({
			setup: () => () => h('div', items.value.map(id => h(MkDeferredMount, { key: id, defer: m.shouldDefer(id), 'data-id': id }, () => h(Content, { label: id })))),
		});
		const result = render(List);
		const isPlaceholder = (id: string) => {
			const el = result.container.querySelector(`[data-id="${id}"]`);
			return el?.tagName !== 'ARTICLE' && el?.querySelector('article') == null;
		};
		return { items, m, isPlaceholder };
	}

	test('defers only the items added by fetchOlder', async () => {
		const { items, m, isPlaceholder } = renderList();
		await m.fetchOlder();
		await nextTick();
		expect(['n5', 'n4'].map(isPlaceholder)).toEqual([false, false]);
		expect(['n3', 'n2'].map(isPlaceholder)).toEqual([true, true]);

		// 新着 (fetchOlder の外で足した分) はすぐに出す
		items.value.unshift('n9');
		await nextTick();
		expect(isPlaceholder('n9')).toBe(false);
		// defer は mount のときに 1 度だけ読む (描き直しで外れても、順番が来るまで枠のまま)
		expect(['n3', 'n2'].map(isPlaceholder)).toEqual([true, true]);

		await grantAll();
		expect(['n3', 'n2'].map(isPlaceholder)).toEqual([false, false]);
	});

	test('does not defer a new item that arrives while the fetch waits', async () => {
		const items = ref(['n5', 'n4']);
		let resolve!: () => void;
		const m = deferOlderMounts(() => new Promise<void>(r => { resolve = r; }), () => items.value);
		const List = defineComponent({
			setup: () => () => h('div', items.value.map(id => h(MkDeferredMount, { key: id, defer: m.shouldDefer(id), 'data-id': id }, () => h(Content, { label: id })))),
		});
		const { container } = render(List);
		const done = m.fetchOlder();
		items.value.unshift('n9');
		await nextTick();
		expect(container.querySelector('[data-id="n9"]')?.tagName).toBe('ARTICLE');
		resolve();
		await done;
	});
});

describe('MkDeferredMount with unmountWhenFar', () => {
	// 一覧の FarItems の代わりに、テストから遠い・近いを報告できる偽物を渡す
	function fakeFarItems() {
		const callbacks = new Map<Element, FarItemCallback>();
		const touched = new Set<Element>();
		const far: FarItems = {
			connect() {},
			pause() {},
			resume() {},
			disconnect() {},
			observe(el, cb) {
				callbacks.set(el, cb);
				return () => callbacks.delete(el);
			},
			isTouched: (el) => touched.has(el),
		};
		return { far, callbacks, touched };
	}

	function renderFar(content: () => ReturnType<typeof h>, far: FarItems) {
		return render(() => h(MkDeferredMount, { unmountWhenFar: true, class: 'note' }, content), { global: { provide: { [FAR_ITEMS as symbol]: far } } });
	}

	test('replaces the content with an empty box of the same height when far, and brings it back when near', async () => {
		const f = fakeFarItems();
		const { container } = renderFar(() => h(Content, { label: 'A' }), f.far);
		const wrapper = container.firstElementChild as HTMLElement;
		expect(wrapper.querySelector('article')?.textContent).toBe('A');

		f.callbacks.get(wrapper)!(true, 234);
		await nextTick();
		expect(wrapper.querySelector('article')).toBeNull();
		expect(wrapper.style.height).toBe('234px');
		// 根の要素は替えない (属性も残る)
		expect(container.firstElementChild).toBe(wrapper);
		expect(wrapper.classList.contains('note')).toBe(true);

		f.callbacks.get(wrapper)!(false, 234);
		await nextTick();
		// 戻すのも順番を待つ
		expect(wrapper.querySelector('article')).toBeNull();
		await grantAll();
		expect(wrapper.querySelector('article')?.textContent).toBe('A');
		expect(wrapper.style.height).toBe('');
	});

	test('does not bring the content back if it goes far again before its turn', async () => {
		const f = fakeFarItems();
		const { container } = renderFar(() => h(Content, { label: 'A' }), f.far);
		const wrapper = container.firstElementChild as HTMLElement;
		f.callbacks.get(wrapper)!(true, 234);
		await nextTick();
		f.callbacks.get(wrapper)!(false, 234);
		f.callbacks.get(wrapper)!(true, 234);
		expect(sched.cancelled).toBe(1);
		await nextTick();
		expect(wrapper.querySelector('article')).toBeNull();
	});

	test('keeps an item the user touched', async () => {
		const f = fakeFarItems();
		const { container } = renderFar(() => h(Content, { label: 'A' }), f.far);
		const wrapper = container.firstElementChild as HTMLElement;
		f.touched.add(wrapper);
		f.callbacks.get(wrapper)!(true, 234);
		await nextTick();
		expect(wrapper.querySelector('article')).not.toBeNull();
	});

	test('keeps an item that has the focus', async () => {
		const f = fakeFarItems();
		const { container } = renderFar(() => h('div', [h('button', 'B')]), f.far);
		const wrapper = container.firstElementChild as HTMLElement;
		wrapper.querySelector('button')!.focus();
		f.callbacks.get(wrapper)!(true, 234);
		await nextTick();
		expect(wrapper.querySelector('button')).not.toBeNull();
	});

	test('does not make an empty box of no height (the list is hidden)', async () => {
		const f = fakeFarItems();
		const { container } = renderFar(() => h(Content, { label: 'A' }), f.far);
		const wrapper = container.firstElementChild as HTMLElement;
		f.callbacks.get(wrapper)!(true, 0);
		await nextTick();
		expect(wrapper.querySelector('article')).not.toBeNull();
	});

	test('keeps the first height while it stays far', async () => {
		const f = fakeFarItems();
		const { container } = renderFar(() => h(Content, { label: 'A' }), f.far);
		const wrapper = container.firstElementChild as HTMLElement;
		f.callbacks.get(wrapper)!(true, 234);
		await nextTick();
		// 空の箱になった後の高さ (箱そのもの) で上書きしない
		f.callbacks.get(wrapper)!(true, 999);
		await nextTick();
		expect(wrapper.style.height).toBe('234px');
	});

	test('ignores far while it still waits for its first turn', async () => {
		const f = fakeFarItems();
		const { container } = render(() => h(MkDeferredMount, { defer: true, unmountWhenFar: true }, () => h(Content, { label: 'A' })), { global: { provide: { [FAR_ITEMS as symbol]: f.far } } });
		const wrapper = container.firstElementChild as HTMLElement;
		// 枠 (160px) の高さで箱にすると、中身の高さと食い違う
		f.callbacks.get(wrapper)!(true, 160);
		await grantAll();
		expect(wrapper.querySelector('article')?.textContent).toBe('A');
		expect(wrapper.style.height).toBe('');
	});

	test('cancels a pending remount when unmounted', async () => {
		const f = fakeFarItems();
		const { container, unmount } = renderFar(() => h(Content), f.far);
		const wrapper = container.firstElementChild as HTMLElement;
		f.callbacks.get(wrapper)!(true, 234);
		await nextTick();
		f.callbacks.get(wrapper)!(false, 234);
		expect(sched.cancelled).toBe(0);
		unmount();
		expect(sched.cancelled).toBe(1);
	});

	test('runs the item disposers when the item goes away, not when its content does', async () => {
		const f = fakeFarItems();
		const disposed = vi.fn();
		const Registering = defineComponent({
			setup() {
				inject(ITEM_DISPOSE)!(disposed);
				return () => h('article', 'R');
			},
		});
		const { container, unmount } = renderFar(() => h(Registering), f.far);
		const wrapper = container.firstElementChild as HTMLElement;
		f.callbacks.get(wrapper)!(true, 100);
		await nextTick();
		expect(disposed).not.toHaveBeenCalled();
		unmount();
		expect(disposed).toHaveBeenCalledOnce();
	});

	test('keeps an item that is playing media', async () => {
		const f = fakeFarItems();
		const { container } = renderFar(() => h('div', [h('video')]), f.far);
		const wrapper = container.firstElementChild as HTMLElement;
		Object.defineProperty(wrapper.querySelector('video')!, 'paused', { value: false });
		f.callbacks.get(wrapper)!(true, 234);
		await nextTick();
		expect(wrapper.querySelector('video')).not.toBeNull();
	});

	test('keeps the item state across the remount', async () => {
		const f = fakeFarItems();
		const states: Map<string, unknown>[] = [];
		const Stateful = defineComponent({
			setup() {
				states.push(inject(ITEM_STATE)!);
				return () => h('article', 'S');
			},
		});
		const { container } = renderFar(() => h(Stateful), f.far);
		const wrapper = container.firstElementChild as HTMLElement;
		f.callbacks.get(wrapper)!(true, 100);
		await nextTick();
		f.callbacks.get(wrapper)!(false, 100);
		await grantAll();
		expect(states).toHaveLength(2);
		expect(states[0]).toBeInstanceOf(Map);
		expect(states[1]).toBe(states[0]);
	});

	test('stops watching when unmounted', () => {
		const f = fakeFarItems();
		const { unmount } = renderFar(() => h(Content), f.far);
		expect(f.callbacks.size).toBe(1);
		unmount();
		expect(f.callbacks.size).toBe(0);
	});
});
