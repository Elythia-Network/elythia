/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, describe, expect, it } from 'vitest';
import { BroadcastChannel as LibraryChannel } from 'broadcast-channel';
import { TypedBroadcastChannel, microSeconds } from '@/utility/broadcast-channel.js';

// ライブラリ broadcast-channel と同じ形で使え、同じ形のメッセージを送受信すること (#3613)。
// デプロイの前に開いたタブ (ライブラリの版) と後のタブが混在しても、同期が壊れない。
const closers: (() => unknown)[] = [];
afterEach(async () => {
	for (const close of closers.splice(0)) await close();
});
function open<T>(name: string) {
	const ch = new TypedBroadcastChannel<T>(name);
	closers.push(() => ch.close());
	return ch;
}
function openNative(name: string) {
	const ch = new BroadcastChannel(name);
	closers.push(() => ch.close());
	return ch;
}
function openLibrary<T>(name: string) {
	const ch = new LibraryChannel<T>(name, { type: 'native' });
	closers.push(() => ch.close());
	return ch;
}
function nextMessage<T>(subscribe: (fn: (msg: T) => void) => void): Promise<T> {
	return new Promise(resolve => subscribe(resolve));
}
const tick = () => new Promise(resolve => setTimeout(resolve, 30));

describe('TypedBroadcastChannel', () => {
	it('delivers the message itself to the other channels of the same name, not to itself', async () => {
		const sender = open<{ key: string; value: number }>('t3613-basic');
		const receiver = open<{ key: string; value: number }>('t3613-basic');
		const other = open<{ key: string; value: number }>('t3613-basic-other');
		const gotOther: unknown[] = [];
		const gotSelf: unknown[] = [];
		other.addEventListener('message', msg => gotOther.push(msg));
		sender.addEventListener('message', msg => gotSelf.push(msg));
		const received = nextMessage<{ key: string; value: number }>(fn => receiver.addEventListener('message', fn));

		await expect(sender.postMessage({ key: 'k', value: 1 })).resolves.toBeUndefined();
		expect(await received).toEqual({ key: 'k', value: 1 });
		await tick();
		expect(gotOther).toEqual([]);
		expect(gotSelf).toEqual([]);
	});

	it('sends the envelope of the broadcast-channel package', async () => {
		const sender = open<string | null>('t3613-envelope');
		const raw = openNative('t3613-envelope');
		const received = nextMessage<MessageEvent>(fn => raw.addEventListener('message', fn));
		const before = microSeconds();
		await sender.postMessage(null);
		const data = (await received).data;
		expect(data).toEqual({ time: expect.any(Number), type: 'message', data: null });
		expect(data.time).toBeGreaterThan(before);
	});

	it('a tab running the library receives what this sends (new -> old)', async () => {
		const sender = open<string | null>('t3613-to-library');
		const library = openLibrary<string | null>('t3613-to-library');
		const received = nextMessage<string | null>(fn => library.addEventListener('message', fn));
		await tick();
		await sender.postMessage('/settings');
		expect(await received).toBe('/settings');
	});

	it('receives what a tab running the library sends (old -> new)', async () => {
		const library = openLibrary<string | null>('t3613-from-library');
		const receiver = open<string | null>('t3613-from-library');
		const received = nextMessage<string | null>(fn => receiver.addEventListener('message', fn));
		await library.postMessage(null);
		// 封筒ごと渡すと、一斉リロードが `/[object Object]` へ移動していた
		expect(await received).toBeNull();
	});

	it('ignores other envelope types, foreign shapes and messages sent before listening', async () => {
		const receiver = open<unknown>('t3613-ignore');
		const raw = openNative('t3613-ignore');
		const got: unknown[] = [];
		receiver.addEventListener('message', msg => got.push(msg));
		raw.postMessage({ time: microSeconds(), type: 'internal', data: 'internal' });
		raw.postMessage('/bare-value');
		raw.postMessage({ time: 1, type: 'message', data: 'too old' });
		raw.postMessage({ time: microSeconds() + 1000, type: 'message', data: 'ok' });
		await tick();
		expect(got).toEqual(['ok']);
	});

	it('rejects instead of throwing for a message that cannot be cloned', async () => {
		const ch = open<unknown>('t3613-clone');
		let result: Promise<void> | undefined;
		expect(() => { result = ch.postMessage({ fn: () => 1 }); }).not.toThrow();
		await expect(result).rejects.toBeDefined();
	});
});
