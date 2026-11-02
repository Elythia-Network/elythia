/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// タイムラインと通知の一覧が、古いものを読み足した分だけ中身の組み立てを後回しにすることを
// 固定する (#3937)。部品とスケジューラは別のテストで見ているので、ここではつなぎ方を見る。

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterAll, afterEach, beforeAll, describe, expect, test, vi } from 'vitest';
import * as sass from 'sass-embedded';
import { cleanup, fireEvent, render } from '@testing-library/vue';
import { nextTick } from 'vue';
import type { Component } from 'vue';
import { directives } from '@/directives/index.js';
import { components } from '@/components/index.js';
import { mountScheduler } from '@/utility/mount-scheduler.js';
import { prefer } from '@/preferences.js';

vi.mock('@/components/MkNote.vue', async () => {
	const { defineComponent: dc, h: hh } = await import('vue');
	return {
		__esModule: true,
		default: dc({
			props: { note: { type: Object, required: true } },
			setup: (props) => () => hh('div', { class: 'stub-note', 'data-note-id': (props.note as { id: string }).id }),
		}),
	};
});

vi.mock('@/components/MkNotification.vue', async () => {
	const { defineComponent: dc, h: hh } = await import('vue');
	return {
		__esModule: true,
		default: dc({
			props: { notification: { type: Object, required: true } },
			setup: (props) => () => hh('div', { class: 'stub-notification', 'data-notification-id': (props.notification as { id: string }).id }),
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

const id = (prefix: string, n: number) => `${prefix}${String(n).padStart(6, '0')}`;
// 読み足した分の途中で日をまたがせて、日付の区切りの枝も通す
const createdAtOf = (n: number) => new Date(Date.now() - (n < 70 ? 2 * 86400_000 : 0)).toISOString();

function note(n: number) {
	return {
		id: id('note', n), createdAt: createdAtOf(n), userId: 'someone', user: { id: 'someone', username: 'someone', host: null },
		text: 'hi', visibility: 'public', reactions: {}, reactionEmojis: {}, reactionCount: 0, renoteCount: 0, repliesCount: 0, files: [], fileIds: [],
	};
}

function notification(n: number) {
	return { id: id('notif', n), createdAt: createdAtOf(n), type: 'follow', userId: 'someone', user: { id: 'someone', username: 'someone', host: null } };
}

// 最初は新しい 15 件、untilId を付けた読み足しには、その続きの古い 30 件を返す
const realFetch = globalThis.fetch;
beforeAll(() => {
	globalThis.fetch = (async (_url: unknown, init?: RequestInit) => {
		const body = JSON.parse(String(init?.body ?? '{}')) as { untilId?: string };
		const make = String(_url).includes('notifications') ? notification : note;
		const from = body.untilId == null ? 100 : 85;
		const count = body.untilId == null ? 15 : 30;
		const items = Array.from({ length: count }, (_, i) => make(from - i));
		return new Response(JSON.stringify(items), { status: 200, headers: { 'content-type': 'application/json' } });
	}) as typeof globalThis.fetch;
});

afterAll(() => {
	globalThis.fetch = realFetch;
});

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
});

async function flushPromises(): Promise<void> {
	for (let i = 0; i < 5; i++) {
		await new Promise<void>((resolve) => setTimeout(resolve, 0));
		await nextTick();
	}
}

async function waitFor(check: () => boolean): Promise<void> {
	for (let i = 0; i < 100 && !check(); i++) {
		await new Promise<void>((resolve) => setTimeout(resolve, 20));
	}
}

async function mountList(component: Component, props: Record<string, unknown>) {
	// 広告の枝も通る (読み足しの 11 件目)。広告の中身はここでは見ないので差し替える
	const result = render(component, { props, global: { directives, components, stubs: { MkAd: true } } });
	await flushPromises();
	return result;
}

function moreButton(container: Element): HTMLElement {
	const button = [...container.querySelectorAll<HTMLElement>('button')].find(b => [...b.classList].some(c => c.includes('more')));
	if (button == null) throw new Error('more button not rendered');
	return button;
}

describe.each([
	{ name: 'MkStreamingNotesTimeline', load: () => import('@/components/MkStreamingNotesTimeline.vue'), props: { src: 'home' }, selector: '.stub-note' },
	{ name: 'MkStreamingNotificationsTimeline', load: () => import('@/components/MkStreamingNotificationsTimeline.vue'), props: {}, selector: '.stub-notification' },
])('$name', ({ load, props, selector }) => {
	test('defers only the items added by "load more"', async () => {
		const request = vi.spyOn(mountScheduler, 'request');
		const { container } = await mountList((await load()).default, props);
		expect(container.querySelectorAll(selector)).toHaveLength(15);
		// 最初の読み込みは後回しにしない
		expect(request).not.toHaveBeenCalled();

		await fireEvent.click(moreButton(container));
		await flushPromises();
		expect(request).toHaveBeenCalledTimes(30);

		await waitFor(() => container.querySelectorAll(selector).length === 45);
		expect(container.querySelectorAll(selector)).toHaveLength(45);
	});

	test('defers the items added when "load more" comes into view (infinite scroll)', async () => {
		// 既定の設定 (enableInfiniteScroll) では、ボタンが画面に入ると v-appear が読み足す
		const observed = new Map<Element, IntersectionObserverCallback>();
		vi.stubGlobal('IntersectionObserver', class {
			constructor(private cb: IntersectionObserverCallback) {}
			observe(el: Element) {
				observed.set(el, this.cb);
			}
			unobserve() {}
			disconnect() {}
		});
		const infinite = prefer.s.enableInfiniteScroll;
		(prefer.s as Record<string, unknown>).enableInfiniteScroll = true;
		try {
			const request = vi.spyOn(mountScheduler, 'request');
			const { container } = await mountList((await load()).default, props);
			const appear = observed.get(moreButton(container));
			expect(appear, 'v-appear observes the button').toBeDefined();

			appear!([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver);
			await flushPromises();
			expect(request).toHaveBeenCalledTimes(30);
		} finally {
			(prefer.s as Record<string, unknown>).enableInfiniteScroll = infinite;
			vi.unstubAllGlobals();
		}
	});
});

describe('MkStreamingNotesTimeline styles', () => {
	// テストの環境では CSS Modules の style が注入されないので、SFC の style を実際にコンパイルして見る
	const source = readFileSync(join(__dirname, '../../src/components/MkStreamingNotesTimeline.vue'), 'utf8');
	const css = sass.compileString(source.match(/<style lang="scss" module>([\s\S]*?)<\/style>/)?.[1] ?? '').css;

	test('does not draw a divider for a deferred note that renders nothing', () => {
		// ハードミュートした MkNote は空の div を描く。後回しにすると外側の div に .note が付く
		const rule = css.match(/\.note:not\(:empty\)[^{]*\{[^}]*border-bottom/)?.[0] ?? '';
		expect(rule).toContain(':not(:has(> :only-child:empty))');
	});
});
