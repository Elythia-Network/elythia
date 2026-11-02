/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// タイムラインと通知の一覧が、古いものを読み足した分だけ中身の組み立てを後回しにすることを
// 固定する (#3937)。部品とスケジューラは別のテストで見ているので、ここではつなぎ方を見る。

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, test, vi } from 'vitest';
import * as sass from 'sass-embedded';
import { cleanup, fireEvent, render } from '@testing-library/vue';
import { nextTick } from 'vue';
import type { Component } from 'vue';
import { directives } from '@/directives/index.js';
import { components } from '@/components/index.js';
import { mountScheduler } from '@/utility/mount-scheduler.js';
import { prefer } from '@/preferences.js';
import { globalEvents } from '@/events.js';

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

// 新着を流せるよう、チャンネルを名前で引けるようにしておく
const channels = vi.hoisted(() => new Map<string, { emit: (ev: string, ...args: unknown[]) => void }>());
vi.mock('@/stream.js', async () => {
	const { EventEmitter: EE } = await import('eventemitter3');
	const stream = new EE() as InstanceType<typeof EE> & Record<string, unknown>;
	stream.useChannel = (name: string) => {
		const conn = new EE() as InstanceType<typeof EE> & Record<string, unknown>;
		conn.dispose = () => {};
		conn.send = () => {};
		channels.set(name, conn);
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
		const from = body.untilId == null ? 100 : Number(body.untilId.replace(/\D/g, '')) - 1;
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

async function mountList(component: Component, props: Record<string, unknown>, { realTransitionGroup = false } = {}) {
	// 広告の枝も通る (読み足しの 11 件目)。広告の中身はここでは見ないので差し替える。
	// @testing-library/vue は既定で TransitionGroup をスタブにするので、遷移を見るときは本物にする
	const stubs: Record<string, boolean> = realTransitionGroup ? { MkAd: true, 'transition-group': false } : { MkAd: true };
	const result = render(component, { props, global: { directives, components, stubs } });
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

// 一覧に入った要素と、付いたクラスを全部記録する。出現の遷移のクラスは入れる前に付き、
// 次のフレーム以降に外れるので、終わった後に DOM を見ても分からない
function recordClasses(root: Element): { seen: () => string[]; stop: () => void } {
	const seen: string[] = [];
	const observer = new MutationObserver(records => {
		for (const r of records) {
			if (r.type === 'attributes' && r.oldValue != null) seen.push(r.oldValue);
			for (const n of r.addedNodes) if (n instanceof Element) seen.push(n.className);
			if (r.target instanceof Element) seen.push(r.target.className);
		}
	});
	observer.observe(root, { subtree: true, childList: true, attributes: true, attributeFilter: ['class'], attributeOldValue: true });
	return { seen: () => seen, stop: () => observer.disconnect() };
}

const enterClass = (classes: string[]) => classes.some(c => c.includes('transition_x_enter'));
const movedWith = (spy: { mock: { calls: unknown[][] } }) => spy.mock.calls.some(([el]) => el instanceof Element && el.className.includes('transition_x_move'));

describe.each([
	{ name: 'MkStreamingNotesTimeline', load: () => import('@/components/MkStreamingNotesTimeline.vue'), props: { src: 'home' }, channel: 'homeTimeline', event: 'note', make: note },
	{ name: 'MkStreamingNotificationsTimeline', load: () => import('@/components/MkStreamingNotificationsTimeline.vue'), props: {}, channel: 'main', event: 'notification', make: notification },
])('$name with animation', ({ load, props, channel, event, make }) => {
	let animation: unknown;
	beforeEach(() => {
		animation = prefer.s.animation;
		(prefer.s as Record<string, unknown>).animation = true;
	});
	afterEach(() => {
		(prefer.s as Record<string, unknown>).animation = animation;
	});

	test('adds older items without the enter transition', async () => {
		const { container } = await mountList((await load()).default, props, { realTransitionGroup: true });
		const record = recordClasses(container);
		const rect = vi.spyOn(Element.prototype, 'getBoundingClientRect');
		const computedStyle = vi.spyOn(window, 'getComputedStyle');

		await fireEvent.click(moreButton(container));
		await flushPromises();
		// 出現の遷移は、終わりを知るために 2 フレーム後に getComputedStyle を読む
		await new Promise(r => setTimeout(r, 100));
		record.stop();

		expect(enterClass(record.seen())).toBe(false);
		// 以前は読み足した 30 件それぞれで読んでいた。移動の判定 (TransitionGroup が描いた
		// 後に 1 回読む) だけが残る
		expect(computedStyle.mock.calls.length).toBeLessThanOrEqual(1);
		// 移動の判定には、移動のクラスを付けた複製を使う。元の移動のクラスのままだと、ブラウザ
		// では移動ありと判定され、描いた後に全件の位置を測り直す (この環境は CSS が無いので、
		// 判定の結果ではなく、渡したクラスで見る)
		expect(movedWith(computedStyle)).toBe(false);
		// 一覧の描き直しを増やさない。描き直すたびに、描く前の全件 (15 件) の位置を測る
		expect(rect.mock.calls.length).toBeLessThanOrEqual(15);

		// 続けて読み足しても、読み足しを始めるときに描き直さない
		rect.mockClear();
		await fireEvent.click(moreButton(container));
		await flushPromises();
		expect(container.querySelectorAll('[data-scroll-anchor]').length + container.querySelectorAll('.stub-notification').length).toBeGreaterThan(45);
		expect(rect.mock.calls.length).toBeLessThanOrEqual(45);
	});

	test('still adds a new item at the top with the enter transition', async () => {
		const { container } = await mountList((await load()).default, props, { realTransitionGroup: true });
		await fireEvent.click(moreButton(container));
		await flushPromises();

		const record = recordClasses(container);
		const computedStyle = vi.spyOn(window, 'getComputedStyle');
		channels.get(channel)!.emit(event, make(200));
		await nextTick();
		await Promise.resolve();
		record.stop();
		expect(enterClass(record.seen())).toBe(true);
		// 移動のクラスも戻っている
		expect(movedWith(computedStyle)).toBe(true);
	});
});

describe('MkStreamingNotesTimeline with animation, after a reload', () => {
	let animation: unknown;
	beforeEach(() => {
		animation = prefer.s.animation;
		(prefer.s as Record<string, unknown>).animation = true;
	});
	afterEach(() => {
		(prefer.s as Record<string, unknown>).animation = animation;
	});

	const leaveClass = (classes: string[]) => classes.some(c => c.includes('transition_x_leave'));

	async function deleteNote(container: Element, n: number): Promise<string[]> {
		const record = recordClasses(container);
		globalEvents.emit('noteDeleted', id('note', n));
		await nextTick();
		await Promise.resolve();
		record.stop();
		return record.seen();
	}

	test('keeps transitions off until the newest item changes', async () => {
		const { default: Timeline } = await import('@/components/MkStreamingNotesTimeline.vue');
		const { container } = await mountList(Timeline, { src: 'home' }, { realTransitionGroup: true });
		await fireEvent.click(moreButton(container));
		await flushPromises();
		// 先頭 (100) 以外の 2 件目 (99) と末尾 (56) が消えても戻さない。先頭を見ていなければ、
		// ここで戻って消える動きが付く
		expect(leaveClass(await deleteNote(container, 99))).toBe(false);
		expect(leaveClass(await deleteNote(container, 56))).toBe(false);
		// 先頭が消えたら戻す
		expect(leaveClass(await deleteNote(container, 100))).toBe(true);
	});

	test('restores transitions when the list is reloaded with the same newest item', async () => {
		const { default: Timeline } = await import('@/components/MkStreamingNotesTimeline.vue');
		const { container, rerender } = await mountList(Timeline, { src: 'home' }, { realTransitionGroup: true });
		await fireEvent.click(moreButton(container));
		await flushPromises();
		// 読み足した後は、途中の削除にも動きを付けない (先頭が変わるまで)
		expect(leaveClass(await deleteNote(container, 95))).toBe(false);

		// 再読み込み (fetch の mock は同じ 15 件を返すので、先頭の id は変わらない)
		await rerender({ src: 'home', withSensitive: false });
		await flushPromises();
		expect(leaveClass(await deleteNote(container, 94))).toBe(true);
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
