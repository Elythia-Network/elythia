/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// タイムラインと通知の一覧が、画面から遠い項目を外し、近付いたら戻すことを固定する (#3941)。
// 仕組みと部品は別のテストで見ているので、ここではつなぎ方を見る。

import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, test, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/vue';
import { defineComponent, h, KeepAlive, nextTick, ref } from 'vue';
import type { Component } from 'vue';
import { directives } from '@/directives/index.js';
import { components } from '@/components/index.js';

vi.mock('@/components/MkNote.vue', async () => {
	const { defineComponent: dc, h: hh } = await import('vue');
	return {
		__esModule: true,
		default: dc({
			props: { note: { type: Object, required: true } },
			setup: (props) => () => hh('div', { class: 'stub-item', 'data-id': (props.note as { id: string }).id }, [hh('button', 'react')]),
		}),
	};
});

vi.mock('@/components/MkNotification.vue', async () => {
	const { defineComponent: dc, h: hh } = await import('vue');
	return {
		__esModule: true,
		default: dc({
			props: { notification: { type: Object, required: true } },
			setup: (props) => () => hh('div', { class: 'stub-item', 'data-id': (props.notification as { id: string }).id }, [hh('button', 'open')]),
		}),
	};
});

vi.mock('@/stream.js', async () => {
	const { EventEmitter: EE } = await import('eventemitter3');
	const stream = new EE() as InstanceType<typeof EE> & Record<string, unknown>;
	stream.useChannel = () => {
		const conn = new EE() as InstanceType<typeof EE> & Record<string, unknown>;
		conn.dispose = () => {};
		conn.send = () => {};
		return conn;
	};
	stream.send = () => {};
	return { useStream: () => stream };
});

// IntersectionObserver を、テストから交差を報告できる偽物にする
class FakeObserver {
	static instances: FakeObserver[] = [];
	observed = new Set<Element>();
	disconnected = false;
	constructor(public callback: IntersectionObserverCallback) {
		FakeObserver.instances.push(this);
	}
	observe(el: Element) {
		this.observed.add(el);
	}
	unobserve(el: Element) {
		this.observed.delete(el);
	}
	disconnect() {
		this.disconnected = true;
		this.observed.clear();
	}
}
// v-appear なども IntersectionObserver を使うので、遠い項目の判定に使っているもの (項目を見ているもの) を探す
const farObservers = () => FakeObserver.instances.filter(o => !o.disconnected && [...o.observed].some(el => el.hasAttribute('data-far-item')));

function report(el: Element, isIntersecting: boolean, height = 120) {
	for (const o of farObservers()) {
		if (o.observed.has(el)) o.callback([{ target: el, isIntersecting, boundingClientRect: { height } as DOMRectReadOnly } as IntersectionObserverEntry], o as unknown as IntersectionObserver);
	}
}

const id = (prefix: string, n: number) => `${prefix}${String(n).padStart(6, '0')}`;
const createdAt = new Date(Date.now() - 86400_000).toISOString();
const note = (n: number) => ({
	id: id('note', n), createdAt, userId: 'someone', user: { id: 'someone', username: 'someone', host: null },
	text: 'hi', visibility: 'public', reactions: {}, reactionEmojis: {}, reactionCount: 0, renoteCount: 0, repliesCount: 0, files: [], fileIds: [],
});
const notification = (n: number) => ({ id: id('notif', n), createdAt, type: 'follow', userId: 'someone', user: { id: 'someone', username: 'someone', host: null } });

const realFetch = globalThis.fetch;
beforeAll(() => {
	globalThis.fetch = (async (url: unknown) => {
		const make = String(url).includes('notifications') ? notification : note;
		return new Response(JSON.stringify(Array.from({ length: 15 }, (_, i) => make(100 - i))), { status: 200, headers: { 'content-type': 'application/json' } });
	}) as typeof globalThis.fetch;
});
afterAll(() => {
	globalThis.fetch = realFetch;
});

beforeEach(() => {
	FakeObserver.instances = [];
	vi.stubGlobal('IntersectionObserver', FakeObserver);
});
afterEach(() => {
	cleanup();
	vi.unstubAllGlobals();
});

async function flush(): Promise<void> {
	for (let i = 0; i < 5; i++) {
		await new Promise<void>((resolve) => setTimeout(resolve, 0));
		await nextTick();
	}
}

const wrapperOf = (container: Element, itemId: string) => container.querySelector(`[data-id="${itemId}"]`)?.closest('[data-far-item]') as HTMLElement;

describe.each([
	{ name: 'MkStreamingNotesTimeline', load: () => import('@/components/MkStreamingNotesTimeline.vue'), props: { src: 'home' }, prefix: 'note' },
	{ name: 'MkStreamingNotificationsTimeline', load: () => import('@/components/MkStreamingNotificationsTimeline.vue'), props: {}, prefix: 'notif' },
])('$name', ({ load, props, prefix }) => {
	async function mountList() {
		const result = render((await load()).default, { props, global: { directives, components, stubs: { MkAd: true } } });
		await flush();
		return result;
	}

	test('unmounts an item that goes far and brings it back when it comes near', async () => {
		const { container } = await mountList();
		expect(farObservers()).toHaveLength(1);
		const wrapper = wrapperOf(container, id(prefix, 95));
		expect(wrapper).not.toBeNull();

		report(wrapper, false, 180);
		await nextTick();
		expect(container.querySelector(`[data-id="${id(prefix, 95)}"]`)).toBeNull();
		expect(wrapper.style.height).toBe('180px');
		// ほかの項目はそのまま
		expect(container.querySelector(`[data-id="${id(prefix, 96)}"]`)).not.toBeNull();

		report(wrapper, true, 180);
		await flush();
		await new Promise(r => setTimeout(r, 50));
		expect(container.querySelector(`[data-id="${id(prefix, 95)}"]`)).not.toBeNull();
	});

	test('keeps an item the user touched', async () => {
		const { container } = await mountList();
		const wrapper = wrapperOf(container, id(prefix, 95));
		await fireEvent.click(wrapper.querySelector('button')!);
		report(wrapper, false, 180);
		await nextTick();
		expect(container.querySelector(`[data-id="${id(prefix, 95)}"]`)).not.toBeNull();
	});
});

describe.each([
	{ name: 'MkStreamingNotesTimeline', load: () => import('@/components/MkStreamingNotesTimeline.vue'), props: { src: 'home' } },
	{ name: 'MkStreamingNotificationsTimeline', load: () => import('@/components/MkStreamingNotificationsTimeline.vue'), props: {} },
])('$name lifecycle', ({ load, props }) => {
	test('stops watching while kept alive in the background and starts again when activated', async () => {
		const List = (await load()).default as Component;
		const shown = ref(true);
		const Other = defineComponent({ render: () => h('div', 'other') });
		const Host = defineComponent({
			setup: () => () => h(KeepAlive, null, [shown.value ? h(List, { key: 'tl', ...props }) : h(Other, { key: 'other' })]),
		});
		render(Host, { global: { directives, components, stubs: { MkAd: true } } });
		await flush();
		expect(farObservers()).toHaveLength(1);

		shown.value = false;
		await flush();
		expect(farObservers()).toHaveLength(0);

		shown.value = true;
		await flush();
		expect(farObservers()).toHaveLength(1);
	});

	test('stops watching when unmounted', async () => {
		const List = (await load()).default as Component;
		const { unmount } = render(List, { props, global: { directives, components, stubs: { MkAd: true } } });
		await flush();
		const observers = farObservers();
		expect(observers).toHaveLength(1);
		unmount();
		expect(observers[0].disconnected).toBe(true);
	});
});
