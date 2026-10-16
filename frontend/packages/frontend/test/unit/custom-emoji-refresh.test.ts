/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { nextTick } from 'vue';
import { render, cleanup } from '@testing-library/vue';
import type * as Misskey from 'misskey-js';
import { preferState } from '../setup.unit.js';

vi.mock('@/i.js', () => ({
	$i: { id: 'me', username: 'me', host: null, policies: {} },
	iAmModerator: false,
	iAmAdmin: false,
	notesCount: 0,
}));
vi.mock('@/utility/misskey-api.js', () => ({ misskeyApi: vi.fn(() => new Promise(() => {})), misskeyApiGet: vi.fn(() => new Promise(() => {})) }));
vi.mock('@/utility/idb-proxy.js', () => ({ get: vi.fn(async () => undefined), set: vi.fn() }));

import { components } from '@/components/index.js';
import { directives } from '@/directives/index.js';
import { customEmojis } from '@/custom-emojis.js';
import MkCustomEmoji from '@/components/global/MkCustomEmoji.vue';
import MkReactionsViewer from '@/components/MkReactionsViewer.vue';

function emoji(name: string, url: string): Misskey.entities.EmojiSimple {
	return { name, url, category: null, aliases: [] };
}

beforeEach(() => {
	customEmojis.value = [];
});
afterEach(() => {
	cleanup();
	delete preferState.showAvailableReactionsFirstInNote;
});

// 起動時に手元の一覧で先に描いた絵文字が、取り直した一覧に追従する (#3613)。
describe('MkCustomEmoji follows a refreshed emoji list', () => {
	test('a name drawn before it was known becomes an image, and back', async () => {
		const r = render(MkCustomEmoji, { props: { name: 'added' }, global: { directives, components } });
		expect(r.container.textContent).toBe(':added:');
		expect(r.container.querySelector('img')).toBeNull();

		customEmojis.value = [emoji('added', 'https://example.test/added.png')];
		await nextTick();
		const img = r.container.querySelector('img');
		expect(img).not.toBeNull();
		expect(img?.getAttribute('alt')).toBe(':added:');
		expect(img?.getAttribute('src')).toContain('added.png');

		customEmojis.value = [emoji('added', 'https://example.test/changed.png')];
		await nextTick();
		expect(r.container.querySelector('img')?.getAttribute('src')).toContain('changed.png');

		customEmojis.value = [];
		await nextTick();
		expect(r.container.textContent).toBe(':added:');
	});
});

describe('MkReactionsViewer re-sorts when reactability changes', () => {
	function order(container: Element): string[] {
		return Array.from(container.querySelectorAll('button')).map(b => b.querySelector('img')?.getAttribute('title') ?? '');
	}

	function toggleable(container: Element): boolean[] {
		return Array.from(container.querySelectorAll('button')).map(b => /canToggle/.test(b.className));
	}

	test('a reaction that becomes reactable after the refresh moves to the front', async () => {
		preferState.showAvailableReactionsFirstInNote = true;
		const r = render(MkReactionsViewer, {
			props: { noteId: 'n1', reactions: { ':unknown@.:': 5, ':known@.:': 1 }, reactionEmojis: {}, myReaction: null },
			global: { directives, components },
		});
		await nextTick();
		expect(order(r.container)).toEqual([':unknown:', ':known:']);
		expect(toggleable(r.container)).toEqual([false, false]);

		customEmojis.value = [emoji('known', 'https://example.test/known.png')];
		await nextTick();
		await nextTick();
		expect(order(r.container)).toEqual([':known:', ':unknown:']);
		// チップの「押せる」表示 (MkReactionsViewer.reaction.vue の canToggle) も追従する
		expect(toggleable(r.container)).toEqual([true, false]);
	});

	test('the order is kept when nothing about reactability changed', async () => {
		preferState.showAvailableReactionsFirstInNote = true;
		customEmojis.value = [emoji('known', 'https://example.test/known.png'), emoji('b', 'https://example.test/b.png')];
		await nextTick();
		const r = render(MkReactionsViewer, {
			props: { noteId: 'n1', reactions: { ':b@.:': 5, ':known@.:': 1 }, reactionEmojis: {}, myReaction: null },
			global: { directives, components },
		});
		await nextTick();
		expect(order(r.container)).toEqual([':b:', ':known:']);
		// 並べた後の数の変化では並びを動かさない (本家と同じ)
		await r.rerender({ noteId: 'n1', reactions: { ':b@.:': 5, ':known@.:': 9 }, reactionEmojis: {}, myReaction: null });
		await nextTick();
		expect(order(r.container)).toEqual([':b:', ':known:']);
		// このノートに関係しない絵文字が増えても並べ直さない
		customEmojis.value = [...customEmojis.value, emoji('other', 'https://example.test/other.png')];
		await nextTick();
		await nextTick();
		expect(order(r.container)).toEqual([':b:', ':known:']);
	});
});
