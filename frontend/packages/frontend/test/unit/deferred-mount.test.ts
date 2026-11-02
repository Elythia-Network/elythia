/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 古いものを読み足したときに、中身を後から組み立てる部品を固定する (#3937)。

import { afterEach, describe, expect, test, vi } from 'vitest';
import { cleanup, render } from '@testing-library/vue';
import { defineComponent, h, nextTick, ref, TransitionGroup } from 'vue';

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
		expect(wrapper.className).toContain('placeholder');
		expect(wrapper.classList.contains('note')).toBe(true);
		expect(wrapper.getAttribute('data-scroll-anchor')).toBe('a');

		await grantAll();
		expect(wrapper.querySelector('article')?.textContent).toBe('A');
		expect(wrapper.className).not.toContain('placeholder');
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
