/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { nextTick } from 'vue';
import { render, cleanup } from '@testing-library/vue';
import type { Component, InjectionKey, ShallowRef } from 'vue';
import type * as Elythia from 'elythia-js';

// frontend-embed のモジュールを、この package の vitest で動かす (embed の中の `@/` は
// vitest.config.unit.ts が embed の src に解決する)。型の検査 (vue-tsc) は frontend の
// tsconfig で embed のファイルを辿ると `@/` を frontend の src として読んで落ちるので、
// 動的 import にして、使う分だけ型を書く。
const api = vi.hoisted(() => ({ misskeyApi: vi.fn(), misskeyApiGet: vi.fn() }));
vi.mock('../../../frontend-embed/src/misskey-api.js', () => api);

const embedSrc = join(__dirname, '../../../frontend-embed/src');
const { customEmojis, refreshCustomEmojisOnBoot } = await import(/* @vite-ignore */ `${embedSrc}/custom-emojis.ts`) as {
	customEmojis: ShallowRef<Elythia.entities.EmojiSimple[]>;
	refreshCustomEmojisOnBoot: () => Promise<void>;
};
const { DI } = await import(/* @vite-ignore */ `${embedSrc}/di.ts`) as { DI: { mediaProxy: InjectionKey<unknown> } };
const { default: EmCustomEmoji } = await import(/* @vite-ignore */ `${embedSrc}/components/EmCustomEmoji.vue`) as { default: Component };

function emoji(name: string, url: string): Elythia.entities.EmojiSimple {
	return { name, url, category: null, aliases: [] };
}

function deferred<T>() {
	let resolve!: (v: T) => void;
	const promise = new Promise<T>(r => { resolve = r; });
	return { promise, resolve };
}

beforeEach(() => {
	customEmojis.value = [];
	localStorage.removeItem('lastEmojisFetchedAt');
	localStorage.removeItem('emojis');
	api.misskeyApi.mockReset();
	api.misskeyApiGet.mockReset();
});
afterEach(() => {
	cleanup();
});

// 埋め込みの起動で、手元に一覧があれば /api/emojis を待たずに描く (#3992)。
describe('embed refreshCustomEmojisOnBoot', () => {
	test('does not wait for the request when a cached list exists', async () => {
		customEmojis.value = [emoji('cached', 'https://example.test/cached.png')];
		const res = deferred<{ emojis: Elythia.entities.EmojiSimple[] }>();
		api.misskeyApiGet.mockReturnValue(res.promise);

		let done = false;
		const boot = refreshCustomEmojisOnBoot().then(() => { done = true; });
		await Promise.resolve();
		await Promise.resolve();
		expect(api.misskeyApiGet).toHaveBeenCalledWith('emojis', {});
		expect(done).toBe(true);
		expect(customEmojis.value.map(e => e.name)).toEqual(['cached']);

		// 裏の取得が返ったら差し替わる
		res.resolve({ emojis: [emoji('fresh', 'https://example.test/fresh.png')] });
		await boot;
		await vi.waitFor(() => expect(customEmojis.value.map(e => e.name)).toEqual(['fresh']));
	});

	test('waits for the request when there is no cached list', async () => {
		const res = deferred<{ emojis: Elythia.entities.EmojiSimple[] }>();
		api.misskeyApiGet.mockReturnValue(res.promise);

		let done = false;
		const boot = refreshCustomEmojisOnBoot().then(() => { done = true; });
		for (let i = 0; i < 5; i++) await Promise.resolve();
		expect(done).toBe(false);

		res.resolve({ emojis: [emoji('fresh', 'https://example.test/fresh.png')] });
		await boot;
		expect(done).toBe(true);
		expect(customEmojis.value.map(e => e.name)).toEqual(['fresh']);
	});

	// boot.ts は読み込むと起動してしまうので、呼び出しの形をソースで確かめる
	test('boot.ts uses it instead of waiting for fetchCustomEmojis', () => {
		const source = readFileSync(join(__dirname, '../../../frontend-embed/src/boot.ts'), 'utf8');
		expect(source).toContain('await refreshCustomEmojisOnBoot();');
		expect(source).not.toMatch(/await fetchCustomEmojis\(/);
	});

	test('a failed request does not stop the boot', async () => {
		api.misskeyApiGet.mockRejectedValue(new Error('network'));
		await expect(refreshCustomEmojisOnBoot()).resolves.toBeUndefined();
		expect(customEmojis.value).toEqual([]);
	});
});

// 手元の一覧で先に描いた絵文字が、取り直した一覧に追従する (#3992)。
describe('EmCustomEmoji follows a refreshed emoji list', () => {
	test('a name drawn before it was known becomes an image, and back', async () => {
		const mediaProxy = { getProxiedImageUrl: (url: string) => url };
		const r = render(EmCustomEmoji, {
			props: { name: 'added' },
			global: { provide: { [DI.mediaProxy as symbol]: mediaProxy } },
		});
		expect(r.container.textContent).toBe(':added:');
		expect(r.container.querySelector('img')).toBeNull();

		customEmojis.value = [emoji('added', 'https://example.test/added.png')];
		await nextTick();
		const img = r.container.querySelector('img');
		expect(img).not.toBeNull();
		expect(img?.getAttribute('alt')).toBe(':added:');
		expect(img?.getAttribute('src')).toBe('https://example.test/added.png');

		customEmojis.value = [emoji('added', 'https://example.test/changed.png')];
		await nextTick();
		expect(r.container.querySelector('img')?.getAttribute('src')).toBe('https://example.test/changed.png');

		customEmojis.value = [];
		await nextTick();
		expect(r.container.textContent).toBe(':added:');
	});
});
