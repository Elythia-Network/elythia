/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 画面から遠くて外したノートを戻したときに、リアクションの写しが元に戻らないこと、
// リアルタイムの設定では外していた間の分を 1 回だけ取り直すことを固定する (#3941)。

import { afterAll, afterEach, beforeAll, describe, expect, test, vi } from 'vitest';
import { cleanup, render } from '@testing-library/vue';
import { defineComponent, h, nextTick, onUnmounted, provide, ref } from 'vue';

const mocks = vi.hoisted(() => ({ api: vi.fn(), realtime: true }));
vi.mock('@/utility/misskey-api.js', () => ({
	misskeyApi: (...args: unknown[]) => mocks.api(...args),
	pendingApiRequestsCount: { value: 0 },
}));
vi.mock('@/i.js', async (importOriginal) => ({ ...await importOriginal<typeof import('@/i.js')>(), $i: { id: 'me', policies: {} } }));
vi.mock('@/store.js', () => ({ store: { s: { get realtimeMode() { return mocks.realtime; } } } }));
vi.mock('@/stream.js', async () => {
	const { EventEmitter: EE } = await import('eventemitter3');
	const stream = new EE() as InstanceType<typeof EE> & Record<string, unknown>;
	stream.send = () => {};
	return { useStream: () => stream };
});

// ポーリングの間隔のタイマーはモジュールを読んだときに作られるので、偽のタイマーを入れてから読む
let mod: typeof import('@/composables/use-note-capture.js');
let ITEM_STATE: typeof import('@/utility/far-items.js').ITEM_STATE;
let ITEM_DISPOSE: typeof import('@/utility/far-items.js').ITEM_DISPOSE;
let useNote: typeof import('@/composables/use-note.js').useNote;
let globalEvents: typeof import('@/events.js').globalEvents;
beforeAll(async () => {
	vi.useFakeTimers();
	mod = await import('@/composables/use-note-capture.js');
	({ ITEM_STATE, ITEM_DISPOSE } = await import('@/utility/far-items.js'));
	({ useNote } = await import('@/composables/use-note.js'));
	({ globalEvents } = await import('@/events.js'));
});
afterAll(() => {
	vi.useRealTimers();
});
afterEach(() => {
	cleanup();
	mocks.api.mockReset();
	mocks.realtime = true;
});

const note = {
	id: 'n1',
	createdAt: new Date().toISOString(),
	reactions: {},
	reactionCount: 0,
	reactionEmojis: {},
	myReaction: null,
	poll: null,
} as never;

// 項目の外側の箱 (MkDeferredMount) の代わりに、ITEM_STATE と ITEM_DISPOSE を渡して中身を出し入れする
function renderItem() {
	const shown = ref(true);
	const state = new Map<string, unknown>();
	const Note = defineComponent({
		setup() {
			const { $note } = mod.useNoteCapture({ note, parentNote: null });
			return () => h('span', { 'data-testid': 'count' }, `${$note.reactionCount}|${$note.myReaction ?? ''}`);
		},
	});
	const Item = defineComponent({
		setup() {
			provide(ITEM_STATE, state);
			const disposers: (() => void)[] = [];
			provide(ITEM_DISPOSE, (d) => disposers.push(d));
			onUnmounted(() => disposers.forEach(d => d()));
			return () => (shown.value ? h(Note) : h('div'));
		},
	});
	const result = render(Item);
	const count = () => result.container.querySelector('[data-testid="count"]')?.textContent?.split('|')[0];
	const mine = () => result.container.querySelector('[data-testid="count"]')?.textContent?.split('|')[1];
	return { shown, count, mine, unmount: result.unmount };
}

async function pollTick(): Promise<void> {
	await vi.advanceTimersByTimeAsync(10_000);
	await nextTick();
}

describe('useNoteCapture with an item state', () => {
	test('continues from the kept reactions after a remount', async () => {
		const { shown, count } = renderItem();
		mod.noteEvents.emit('reacted:n1', { userId: 'u1', reaction: ':a@.:' });
		await nextTick();
		expect(count()).toBe('1');

		shown.value = false;
		await nextTick();
		shown.value = true;
		await nextTick();
		// 部品の中にだけ持つと、一覧のデータ (0 件) から作り直して数が戻る
		expect(count()).toBe('1');

		// 戻した後も 1 つずつ増える (購読し直すと二重に数える)
		mod.noteEvents.emit('reacted:n1', { userId: 'u2', reaction: ':b@.:' });
		await nextTick();
		expect(count()).toBe('2');
	});

	test('applies the reactions made elsewhere while unmounted', async () => {
		const { shown, count, mine } = renderItem();
		shown.value = false;
		await nextTick();
		// 同じノートのリノートの項目からリアクションした
		mod.noteEvents.emit('reacted:n1', { userId: 'me', reaction: ':a@.:' });
		shown.value = true;
		await nextTick();
		expect(count()).toBe('1');
		expect(mine()).toBe(':a@.:');
	});

	test('stops listening when the item goes away', async () => {
		const { unmount } = renderItem();
		expect(mod.noteEvents.listenerCount('reacted:n1')).toBe(1);
		unmount();
		expect(mod.noteEvents.listenerCount('reacted:n1')).toBe(0);
	});

	test('fetches the reactions once after a remount in realtime mode', async () => {
		mocks.api.mockResolvedValue([{ id: 'n1', reactions: { ':a@.:': 2, ':b@.:': 1 }, reactionEmojis: {} }]);
		const { shown, count } = renderItem();
		// 最初に出したときは、ストリームで受けるので取り直さない
		await pollTick();
		expect(mocks.api).not.toHaveBeenCalled();

		shown.value = false;
		await nextTick();
		shown.value = true;
		await nextTick();
		await pollTick();
		expect(mocks.api).toHaveBeenCalledWith('notes/show-partial-bulk', { noteIds: ['n1'] });
		expect(count()).toBe('3');

		// 1 回だけ
		mocks.api.mockClear();
		await pollTick();
		expect(mocks.api).not.toHaveBeenCalled();
	});
});

describe('listeners without an item state', () => {
	test('stop when the component goes away', async () => {
		const before = { reacted: mod.noteEvents.listenerCount('reacted:n1'), deleted: globalEvents.listenerCount('noteDeleted') };
		const Note = defineComponent({
			setup() {
				useNote({ note } as never);
				return () => h('span');
			},
		});
		const { unmount } = render(Note);
		expect(mod.noteEvents.listenerCount('reacted:n1')).toBe(before.reacted + 1);
		expect(globalEvents.listenerCount('noteDeleted')).toBe(before.deleted + 1);
		unmount();
		expect(mod.noteEvents.listenerCount('reacted:n1')).toBe(before.reacted);
		expect(globalEvents.listenerCount('noteDeleted')).toBe(before.deleted);
	});

	test('still see a deletion', async () => {
		const Note = defineComponent({
			setup() {
				const { isDeleted } = useNote({ note } as never);
				return () => h('span', { 'data-testid': 'deleted' }, String(isDeleted.value));
			},
		});
		const { container } = render(Note);
		globalEvents.emit('noteDeleted', 'n1');
		await nextTick();
		expect(container.querySelector('[data-testid="deleted"]')?.textContent).toBe('true');
	});
});

describe('useNote with an item state', () => {
	test('listens for deletions once per item, and stops when the item goes away', async () => {
		const before = globalEvents.listenerCount('noteDeleted');
		const shown = ref(true);
		const state = new Map<string, unknown>();
		const Note = defineComponent({
			setup() {
				useNote({ note } as never);
				return () => h('span');
			},
		});
		const Item = defineComponent({
			setup() {
				provide(ITEM_STATE, state);
				const disposers: (() => void)[] = [];
				provide(ITEM_DISPOSE, (d) => disposers.push(d));
				onUnmounted(() => disposers.forEach(d => d()));
				return () => (shown.value ? h(Note) : h('div'));
			},
		});
		const { unmount } = render(Item);
		expect(globalEvents.listenerCount('noteDeleted')).toBe(before + 1);
		shown.value = false;
		await nextTick();
		shown.value = true;
		await nextTick();
		// 戻すたびに増えない
		expect(globalEvents.listenerCount('noteDeleted')).toBe(before + 1);
		unmount();
		expect(globalEvents.listenerCount('noteDeleted')).toBe(before);
	});

	test('keeps a deletion that happened while unmounted', async () => {
		const shown = ref(true);
		const state = new Map<string, unknown>();
		const Note = defineComponent({
			setup() {
				const { isDeleted } = useNote({ note } as never);
				return () => h('span', { 'data-testid': 'deleted' }, String(isDeleted.value));
			},
		});
		const Item = defineComponent({
			setup() {
				provide(ITEM_STATE, state);
				const disposers: (() => void)[] = [];
				provide(ITEM_DISPOSE, (d) => disposers.push(d));
				onUnmounted(() => disposers.forEach(d => d()));
				return () => (shown.value ? h(Note) : h('div'));
			},
		});
		const { container } = render(Item);
		shown.value = false;
		await nextTick();
		globalEvents.emit('noteDeleted', 'n1');
		shown.value = true;
		await nextTick();
		// 部品の中にだけ持つと、戻したときに消えたノートが出る
		expect(container.querySelector('[data-testid="deleted"]')?.textContent).toBe('true');
	});
});
