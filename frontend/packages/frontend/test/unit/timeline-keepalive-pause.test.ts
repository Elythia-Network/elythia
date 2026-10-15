/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// KeepAlive で裏に回ったタイムラインが、新着を描かずに queue へ積むことを固定する。
//
// 裏の一覧は DOM が画面に無いので isTop() が真になり、届いたノートを全部 mount
// していた。ブラウザは document に入っていない <img> でも src を取りに行くので、
// 一覧を離れている間も画像の取得が続いていた。

import { afterAll, afterEach, beforeAll, describe, expect, test, vi } from 'vitest';
import { cleanup, render } from '@testing-library/vue';
import { defineComponent, h, KeepAlive, nextTick, ref } from 'vue';
import type { Component } from 'vue';
import { directives } from '@/directives/index.js';
import { components } from '@/components/index.js';
import { RECHECK_AFTER_ACTIVATED_MS, useTimelineUpdatePause } from '@/composables/use-timeline-update-pause.js';
import { globalEvents } from '@/events.js';

// ノートの中身ではなく「mount されたか」と「<img> が作られたか」だけを見るので、
// MkNote は画像を 1 枚だけ持つ部品に差し替える。
const noteMounts = vi.hoisted(() => ({ count: 0, imgs: 0, notifications: 0 }));

vi.mock('@/components/MkNote.vue', async () => {
	const { defineComponent: dc, h: hh, onMounted } = await import('vue');
	return {
		__esModule: true,
		default: dc({
			props: { note: { type: Object, required: true } },
			setup(props) {
				onMounted(() => {
					noteMounts.count++;
				});
				return () => hh('div', { class: 'stub-note', 'data-note-id': (props.note as { id: string }).id }, [
					hh('img', {
						src: `https://example.com/${(props.note as { id: string }).id}.png`,
						onVnodeMounted: () => {
							noteMounts.imgs++;
						},
					}),
				]);
			},
		}),
	};
});

vi.mock('@/components/MkNotification.vue', async () => {
	const { defineComponent: dc, h: hh, onMounted } = await import('vue');
	return {
		__esModule: true,
		default: dc({
			props: { notification: { type: Object, required: true } },
			setup(props) {
				onMounted(() => {
					noteMounts.notifications++;
				});
				return () => hh('div', { class: 'stub-notification', 'data-notification-id': (props.notification as { id: string }).id });
			},
		}),
	};
});

// ストリーミングは、チャンネルごとに `note` を emit できる偽物にする。
const fakeStream = vi.hoisted(() => {
	const channels = new Map<string, { emit: (ev: string, ...args: unknown[]) => void }>();
	return { channels };
});

vi.mock('@/stream.js', async () => {
	const { EventEmitter: EE } = await import('eventemitter3');
	const stream = new EE() as InstanceType<typeof EE> & Record<string, unknown>;
	stream.useChannel = (name: string) => {
		const conn = new EE() as InstanceType<typeof EE> & Record<string, unknown>;
		conn.dispose = () => {
			fakeStream.channels.delete(name);
		};
		conn.send = () => {};
		fakeStream.channels.set(name, conn);
		return conn;
	};
	stream.send = () => {};
	return { useStream: () => stream };
});

const realFetch = globalThis.fetch;

beforeAll(() => {
	// 初回の取得は空で返す。ここで見るのはストリーミングで届いた分の扱いだけ。
	globalThis.fetch = (async () => new Response('[]', {
		status: 200,
		headers: { 'content-type': 'application/json' },
	})) as typeof globalThis.fetch;
});

afterAll(() => {
	globalThis.fetch = realFetch;
});

// document の可視性は、visibilityState を差し替えて visibilitychange を投げる
// (useDocumentVisibility が見ているものそのもの)。
let visibility: DocumentVisibilityState = 'visible';
Object.defineProperty(window.document, 'visibilityState', { configurable: true, get: () => visibility });
function setVisibility(v: DocumentVisibilityState) {
	visibility = v;
	window.document.dispatchEvent(new Event('visibilitychange'));
}

afterEach(() => {
	cleanup();
	noteMounts.count = 0;
	noteMounts.imgs = 0;
	noteMounts.notifications = 0;
	visibility = 'visible';
});

let seq = 0;
function makeNote() {
	seq++;
	const id = `note${String(seq).padStart(6, '0')}`;
	return {
		id,
		createdAt: new Date().toISOString(),
		userId: 'someone',
		user: { id: 'someone', username: 'someone', host: null },
		text: 'hi',
		visibility: 'public',
		reactions: {},
		reactionEmojis: {},
		reactionCount: 0,
		renoteCount: 0,
		repliesCount: 0,
		files: [],
		fileIds: [],
	};
}

// 取得の promise と描画の flush を待つ。
async function flushPromises(): Promise<void> {
	for (let i = 0; i < 5; i++) {
		await new Promise<void>((resolve) => setTimeout(resolve, 0));
		await nextTick();
	}
}

async function waitRecheck(): Promise<void> {
	await new Promise<void>((resolve) => setTimeout(resolve, RECHECK_AFTER_ACTIVATED_MS + 50));
	await flushPromises();
}

const Other = defineComponent({ render: () => h('div', 'other page') });

// KeepAlive の外側にスクロールのコンテナを置く (`getScrollContainer` が見つける
// overflow-y: auto)。happy-dom は offsetTop が 0 なので、isTop() は scrollTop が 0
// かどうかになる。
async function mountHost(component: Component, props: Record<string, unknown>) {
	const shown = ref(true);
	const errors: unknown[] = [];
	const Host = defineComponent({
		setup() {
			return () => h('div', { 'data-testid': 'scroller', style: 'overflow-y: auto; height: 100px;' }, [
				h(KeepAlive, null, [
					shown.value ? h(component, { key: 'tl', ...props }) : h(Other, { key: 'other' }),
				]),
			]);
		},
	});
	const result = render(Host, {
		global: {
			directives,
			components,
			config: {
				errorHandler: (err) => {
					errors.push(err);
				},
			},
		},
	});
	await flushPromises();
	const scroller = result.container.querySelector<HTMLElement>('[data-testid="scroller"]')!;
	return { result, shown, errors, scroller };
}

async function mountNotesHost() {
	const { default: MkStreamingNotesTimeline } = await import('@/components/MkStreamingNotesTimeline.vue');
	return mountHost(MkStreamingNotesTimeline, { src: 'home' });
}

async function mountNotificationsHost() {
	const { default: MkStreamingNotificationsTimeline } = await import('@/components/MkStreamingNotificationsTimeline.vue');
	return mountHost(MkStreamingNotificationsTimeline, {});
}

async function scrollTo(scroller: HTMLElement, top: number, { event = true } = {}) {
	scroller.scrollTop = top;
	if (event) scroller.dispatchEvent(new Event('scroll'));
	await nextTick();
}

function emitNote() {
	const ch = fakeStream.channels.get('homeTimeline');
	expect(ch, 'homeTimeline channel is connected').toBeDefined();
	const note = makeNote();
	ch!.emit('note', note);
	return note;
}

function shownNoteIds(container: Element): string[] {
	return [...container.querySelectorAll<HTMLElement>('.stub-note')].map(x => x.dataset.noteId!);
}

let notificationSeq = 0;
function emitNotification() {
	const ch = fakeStream.channels.get('main');
	expect(ch, 'main channel is connected').toBeDefined();
	notificationSeq++;
	const notification = {
		id: `notif${String(notificationSeq).padStart(6, '0')}`,
		createdAt: new Date().toISOString(),
		type: 'follow',
		userId: 'someone',
		user: { id: 'someone', username: 'someone', host: null },
	};
	ch!.emit('notification', notification);
	return notification;
}

describe('KeepAlive で裏に回ったタイムライン', () => {
	test('表示中は届いたノートを描く', async () => {
		const { result } = await mountNotesHost();
		emitNote();
		await nextTick();
		expect(noteMounts.count).toBe(1);
		expect(result.container.querySelectorAll('.stub-note')).toHaveLength(1);
	});

	test('裏にある間は描かず、画像の要素も作らない', async () => {
		const { shown } = await mountNotesHost();
		emitNote();
		await nextTick();
		expect(noteMounts.count).toBe(1);

		shown.value = false;
		await flushPromises();

		for (let i = 0; i < 10; i++) {
			emitNote();
			await nextTick();
		}
		expect(noteMounts.count).toBe(1);
		expect(noteMounts.imgs).toBe(1);
	});

	test('先頭にいたなら、戻ったときに積んでいた分をまとめて出す', async () => {
		const { result, shown } = await mountNotesHost();
		emitNote();
		await nextTick();

		shown.value = false;
		await flushPromises();
		for (let i = 0; i < 10; i++) emitNote();
		await nextTick();

		shown.value = true;
		await flushPromises();

		expect(result.container.querySelectorAll('.stub-note')).toHaveLength(11);
		expect(noteMounts.count).toBe(11);
	});

	test('裏に回っても購読は切らない (戻った後も届く)', async () => {
		const { result, shown } = await mountNotesHost();
		shown.value = false;
		await flushPromises();
		shown.value = true;
		await flushPromises();

		emitNote();
		await nextTick();
		expect(result.container.querySelectorAll('.stub-note')).toHaveLength(1);
	});

	// 裏にある間に消されたノートは queue に積まれたままなので、queue からも消さないと戻ったときに出る。
	test('裏にある間に消えたノートは、戻っても出さない', async () => {
		const { result, shown } = await mountNotesHost();
		shown.value = false;
		await flushPromises();
		const kept = emitNote();
		const deleted = emitNote();
		globalEvents.emit('noteDeleted', deleted.id);
		shown.value = true;
		await flushPromises();

		expect(shownNoteIds(result.container)).toEqual([kept.id]);
	});

	// 離れる前に下を見ていたなら、戻っても一括では出さず「新しいノート」から出す。
	// 戻った直後の isTop() は位置の復元前で当てにならないので、表示中の最後の scroll で見た位置を使う。
	test('下を見ていて離れたなら、戻っても一括では出さない', async () => {
		const { result, shown, scroller } = await mountNotesHost();
		const first = emitNote();
		await nextTick();
		await scrollTo(scroller, 50);

		shown.value = false;
		await flushPromises();
		emitNote();
		emitNote();
		shown.value = true;
		await flushPromises();
		expect(shownNoteIds(result.container)).toEqual([first.id]);

		// 位置の復元の後に見直しても、先頭ではないので出さない。
		await waitRecheck();
		expect(shownNoteIds(result.container)).toEqual([first.id]);

		// 先頭まで戻ると出る (本家と同じ経路)。
		await scrollTo(scroller, 0);
		await flushPromises();
		expect(shownNoteIds(result.container)).toHaveLength(3);
	});

	test('下を見ていたと記録されていても、戻った後に実際に先頭なら出す', async () => {
		const { result, shown, scroller } = await mountNotesHost();
		emitNote();
		await nextTick();
		await scrollTo(scroller, 50);

		shown.value = false;
		await flushPromises();
		emitNote();
		emitNote();
		// 復元で先頭に戻った (scroll イベントは出ない)。
		await scrollTo(scroller, 0, { event: false });
		shown.value = true;
		await flushPromises();
		expect(shownNoteIds(result.container)).toHaveLength(1);

		await waitRecheck();
		expect(shownNoteIds(result.container)).toHaveLength(3);
	});

	// queue を残したまま新着だけを先頭へ入れると、後で出したときに古い分が上に来る。
	test('先頭で新着を受けたら、残っていた queue も先に出して時系列を保つ', async () => {
		const { result, shown, scroller } = await mountNotesHost();
		const first = emitNote();
		await nextTick();
		await scrollTo(scroller, 50);

		shown.value = false;
		await flushPromises();
		const a = emitNote();
		const b = emitNote();
		await scrollTo(scroller, 0, { event: false });
		shown.value = true;
		await flushPromises();

		const c = emitNote();
		await flushPromises();
		expect(shownNoteIds(result.container)).toEqual([c.id, b.id, a.id, first.id]);
	});

	test('一覧が空のまま document が隠れて戻ったら出す', async () => {
		const { result, errors } = await mountNotesHost();
		setVisibility('hidden');
		await nextTick();
		emitNote();
		setVisibility('visible');
		await flushPromises();

		expect(errors).toEqual([]);
		expect(shownNoteIds(result.container)).toHaveLength(1);
	});
});

describe('KeepAlive で裏に回った通知の一覧', () => {
	test('裏にある間は描かない', async () => {
		const { shown } = await mountNotificationsHost();
		emitNotification();
		await flushPromises();
		expect(noteMounts.notifications).toBe(1);

		shown.value = false;
		await flushPromises();
		emitNotification();
		emitNotification();
		await flushPromises();
		expect(noteMounts.notifications).toBe(1);
	});

	// 通知の一覧は queue の件数を出さないので、下を見ていても戻ったときに出す。
	test('下を見ていて離れても、戻ったときに出す', async () => {
		const { result, shown, scroller } = await mountNotificationsHost();
		emitNotification();
		await flushPromises();
		await scrollTo(scroller, 50);

		shown.value = false;
		await flushPromises();
		emitNotification();
		emitNotification();
		shown.value = true;
		await flushPromises();

		expect(result.container.querySelectorAll('.stub-notification')).toHaveLength(3);
	});

	// 先頭にいるのに queue が残っている (scroll イベント無しで先頭に来た) ときに新着を受けたら、
	// 残りも先に出す。この一覧は件数を出さないので、残すと黙って隠れたままになる。
	test('先頭で新着を受けたら、残っていた queue も先に出す', async () => {
		const { result, scroller } = await mountNotificationsHost();
		emitNotification();
		await flushPromises();
		await scrollTo(scroller, 50);
		emitNotification();
		await flushPromises();
		expect(result.container.querySelectorAll('.stub-notification')).toHaveLength(1);

		await scrollTo(scroller, 0, { event: false });
		emitNotification();
		await flushPromises();
		expect(result.container.querySelectorAll('.stub-notification')).toHaveLength(3);
	});

	test('一覧が空のまま document が隠れて戻ったら出す', async () => {
		const { result, errors } = await mountNotificationsHost();
		setVisibility('hidden');
		await nextTick();
		emitNotification();
		setVisibility('visible');
		await flushPromises();

		expect(errors).toEqual([]);
		expect(result.container.querySelectorAll('.stub-notification')).toHaveLength(1);
	});
});

describe('useTimelineUpdatePause', () => {
	function mountPause(isTop: () => boolean) {
		const calls = { visible: 0, activated: 0 };
		let api: ReturnType<typeof useTimelineUpdatePause> | null = null;
		const Child = defineComponent({
			setup() {
				api = useTimelineUpdatePause({
					isTop,
					onVisibleAtTop: () => { calls.visible++; },
					onActivatedAtTop: () => { calls.activated++; },
				});
				return () => h('div');
			},
		});
		const shown = ref(true);
		const Host = defineComponent({
			setup() {
				return () => h(KeepAlive, null, [shown.value ? h(Child, { key: 'c' }) : h(Other, { key: 'o' })]);
			},
		});
		render(Host);
		return { calls, shown, api: () => api! };
	}

	test('裏にある間は止まり、戻ると再開する', async () => {
		const { shown, api } = mountPause(() => true);
		expect(api().isPausing()).toBe(false);
		shown.value = false;
		await flushPromises();
		expect(api().isPausing()).toBe(true);
		shown.value = true;
		await flushPromises();
		expect(api().isPausing()).toBe(false);
	});

	test('離れたときに先頭でなければ、戻っても一括では出さない', async () => {
		let top = false;
		const { calls, shown, api } = mountPause(() => top);
		await flushPromises();
		api().recordScrollPosition();
		const before = calls.activated;
		shown.value = false;
		await flushPromises();
		top = true; // 切り離された DOM では isTop() が真になる
		shown.value = true;
		await flushPromises();
		expect(calls.activated).toBe(before);
	});

	test('離れたときに先頭なら、戻ったときに出す', async () => {
		const { calls, shown } = mountPause(() => true);
		await flushPromises();
		const before = calls.activated;
		shown.value = false;
		await flushPromises();
		shown.value = true;
		await flushPromises();
		expect(calls.activated).toBe(before + 1);
	});

	test('裏にある間の scroll は位置として記録しない', async () => {
		let top = true;
		const { calls, shown, api } = mountPause(() => top);
		await flushPromises();
		const before = calls.activated;
		shown.value = false;
		await flushPromises();
		top = false;
		api().recordScrollPosition();
		shown.value = true;
		await flushPromises();
		// 記録されていれば先頭でないと見なされ、出さない。
		expect(calls.activated).toBe(before + 1);
	});

	// 裏にある間は DOM が無く isTop() が真になるので、見えるようになっても出さない。
	test('裏にある間に document が見えるようになっても出さない', async () => {
		const { calls, shown } = mountPause(() => true);
		await flushPromises();
		shown.value = false;
		await flushPromises();
		setVisibility('hidden');
		await nextTick();
		setVisibility('visible');
		await flushPromises();
		expect(calls.visible).toBe(0);
	});

	test('document が隠れたまま戻ったら出さず、見えるようになってから出す', async () => {
		const { calls, shown, api } = mountPause(() => true);
		await flushPromises();
		const before = calls.activated;
		shown.value = false;
		await flushPromises();
		setVisibility('hidden');
		await nextTick();
		shown.value = true;
		await waitRecheck();
		expect(calls.activated).toBe(before);
		expect(api().isPausing()).toBe(true);

		setVisibility('visible');
		await flushPromises();
		expect(calls.visible).toBe(1);
		expect(api().isPausing()).toBe(false);
	});
});
