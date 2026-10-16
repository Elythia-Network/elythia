/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// `test/setup-network.unit.ts` が、答えを決めていない API の要求を待たせたままにして
// いることを固定する (#3369)。外れると、mount の中で API を叩く部品を描いたテストが、
// サーバーの無い `localhost:3000` への ECONNREFUSED を unhandled rejection として
// 受けて落ちる (本家の `home.test.ts` に await を挟んだ形)。

import { afterEach, describe, expect, test } from 'vitest';
import { cleanup, render } from '@testing-library/vue';
import * as Misskey from 'misskey-js';
import { directives } from '@/directives/index.js';
import { components } from '@/components/index.js';
import XHome from '@/pages/user/home.vue';
import 'intersection-observer';
import { createHoldingFetch } from '../holding-fetch.js';

const SETTLE_WINDOW_MS = 100;

async function settlesWithin(promise: Promise<unknown>, ms: number): Promise<boolean> {
	let settled = false;
	promise.then(() => { settled = true; }, () => { settled = true; });
	await new Promise((resolve) => setTimeout(resolve, ms));
	return settled;
}

// ファイルの読み込み (collect) の間の要求は待たせない。待たせると、最上位で await
// したファイルが vitest ごと止まったまま終わらない。ここでは await せず、決着したか
// だけを見る (サーバーが無ければ reject、あれば resolve。どちらでもよい)。
let collectTimeRequestSettled = false;
fetch(`${window.location.origin}/api/meta`, { method: 'POST' }).then(
	() => { collectTimeRequestSettled = true; },
	() => { collectTimeRequestSettled = true; },
);

describe('unanswered API requests in unit tests', () => {
	afterEach(() => {
		cleanup();
	});

	test('an API request without a mocked answer stays pending', async () => {
		expect(await settlesWithin(fetch(`${window.location.origin}/api/users/notes`, { method: 'POST' }), SETTLE_WINDOW_MS)).toBe(false);
	});

	test('aborting the request rejects it with the abort reason', async () => {
		const controller = new AbortController();
		const pending = fetch(`${window.location.origin}/api/users/notes`, { method: 'POST', signal: controller.signal });
		controller.abort();
		await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
	});

	// `fetchMock` を通すと中止済みの signal は `fetchMock` が先に投げるので、本体を直接試す。
	test('an already aborted signal rejects immediately', async () => {
		const pending = new Map<string, number>();
		const holdingFetch = createHoldingFetch(async () => new Response(), { collecting: () => false, pending: () => pending });
		const controller = new AbortController();
		controller.abort();
		const request = holdingFetch(`${window.location.origin}/api/users/notes`, { method: 'POST', signal: controller.signal });
		expect(await settlesWithin(request.catch(() => {}), SETTLE_WINDOW_MS)).toBe(true);
		await expect(request).rejects.toMatchObject({ name: 'AbortError' });
		expect(pending.size).toBe(0);
	});

	// テストが失敗したときに出す一覧の元。待っている間は載り、中止したら外れる。
	test('held requests are recorded until aborted', async () => {
		const pending = new Map<string, number>();
		const holdingFetch = createHoldingFetch(async () => new Response(), { collecting: () => false, pending: () => pending });
		const controller = new AbortController();
		const request = holdingFetch(`${window.location.origin}/api/users/notes`, { method: 'POST', signal: controller.signal });
		expect([...pending]).toEqual([[`POST ${window.location.origin}/api/users/notes`, 1]]);
		controller.abort();
		await expect(request).rejects.toMatchObject({ name: 'AbortError' });
		expect(pending.size).toBe(0);
	});

	// 同じ要求が 2 つ待っているとき、片方を中止してももう片方は一覧に残す。
	test('aborting one of two identical requests keeps the other recorded', async () => {
		const pending = new Map<string, number>();
		const holdingFetch = createHoldingFetch(async () => new Response(), { collecting: () => false, pending: () => pending });
		const label = `POST ${window.location.origin}/api/users/notes`;
		const controller = new AbortController();
		const aborted = holdingFetch(`${window.location.origin}/api/users/notes`, { method: 'POST', signal: controller.signal });
		void holdingFetch(`${window.location.origin}/api/users/notes`, { method: 'POST' });
		expect(pending.get(label)).toBe(2);
		controller.abort();
		await expect(aborted).rejects.toMatchObject({ name: 'AbortError' });
		expect([...pending]).toEqual([[label, 1]]);
	});

	test('a request made while the file is being collected is not held', async () => {
		await expect.poll(() => collectTimeRequestSettled, { timeout: 10000, interval: 50 }).toBe(true);
	}, 15000);

	test('an answer set with fetchMock still wins', async () => {
		fetchMock.mockOnceIf((req) => new URL(req.url).pathname === '/api/meta', () => ({ status: 200, body: '{"ok":true}' }));
		const res = await fetch(`${window.location.origin}/api/meta`, { method: 'POST' });
		expect(await res.json()).toEqual({ ok: true });
	});

	// 本家の `home.test.ts` と同じ描き方で、`XFiles` が `users/notes` を叩くまで待つ。
	// ここで ECONNREFUSED が着地すると、vitest が unhandled rejection で落とす。
	test('rendering home.vue and waiting does not surface a network error', async () => {
		render(XHome, {
			props: {
				user: {
					id: 'blobcat',
					name: 'blobcat',
					username: 'blobcat',
					host: null,
					roles: [],
					createdAt: '1970-01-01T00:00:00.000Z',
					fields: [],
					pinnedNotes: [],
					avatarUrl: 'https://example.com/avatar.png',
					avatarDecorations: [],
				} as unknown as Misskey.entities.UserDetailed,
				disableNotes: true,
			},
			global: { directives, components },
		});

		const requested = () => fetchMock.mock.calls.some(([input]) => String(input instanceof Request ? input.url : input).endsWith('/api/users/notes'));
		await expect.poll(requested, { timeout: 20000, interval: 50 }).toBe(true);
		await new Promise((resolve) => setTimeout(resolve, SETTLE_WINDOW_MS));
	}, 30000);
});
