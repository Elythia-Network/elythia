/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { beforeEach, describe, expect, test, vi } from 'vitest';
import type * as Misskey from 'misskey-js';

const h = vi.hoisted(() => ({
	get: vi.fn(async () => []),
	set: vi.fn(),
}));

vi.mock('@/utility/idb-proxy.js', () => ({ get: h.get, set: h.set }));
vi.mock('@/utility/misskey-api.js', () => ({ misskeyApi: vi.fn(), misskeyApiGet: vi.fn() }));

import { customEmojis, removeCustomEmojis, updateCustomEmojis } from '@/custom-emojis.js';

function emoji(name: string, url: string): Misskey.entities.EmojiSimple {
	return { name, url, category: null, aliases: [] };
}

beforeEach(() => {
	customEmojis.value = [];
	h.set.mockClear();
});

describe('updateCustomEmojis', () => {
	test('empty events preserve catalog order and item references', () => {
		const catalog = [emoji('first', '/first.png'), emoji('second', '/second.png')];
		customEmojis.value = catalog;

		updateCustomEmojis([]);

		expect(customEmojis.value).toHaveLength(2);
		expect(customEmojis.value[0]).toBe(catalog[0]);
		expect(customEmojis.value[1]).toBe(catalog[1]);
	});

	test('one matching event updates its catalog entry without adding unmatched names', () => {
		const unchanged = emoji('unchanged', '/unchanged.png');
		const current = emoji('updated', '/old.png');
		const updated = emoji('updated', '/new.png');
		customEmojis.value = [current, unchanged];

		updateCustomEmojis([updated, emoji('not-in-catalog', '/extra.png')]);

		expect(customEmojis.value).toHaveLength(2);
		expect(customEmojis.value[0]).toBe(updated);
		expect(customEmojis.value[1]).toBe(unchanged);
	});

	test('first duplicate event wins for every catalog entry with that name', () => {
		const first = emoji('duplicate', '/first.png');
		const later = emoji('duplicate', '/later.png');
		const firstCatalogEntry = emoji('duplicate', '/old-first.png');
		const secondCatalogEntry = emoji('duplicate', '/old-second.png');
		const unchanged = emoji('unchanged', '/unchanged.png');
		customEmojis.value = [firstCatalogEntry, unchanged, secondCatalogEntry];

		updateCustomEmojis([first, later, emoji('new-name', '/new.png')]);

		expect(customEmojis.value).toHaveLength(3);
		expect(customEmojis.value[0]).toBe(first);
		expect(customEmojis.value[1]).toBe(unchanged);
		expect(customEmojis.value[2]).toBe(first);
	});

	test('event names are indexed instead of reread for every catalog entry', () => {
		let eventNameReads = 0;
		const events = Array.from({ length: 20 }, (_, index) => ({
			aliases: [],
			category: null,
			url: `/event-${index}.png`,
			get name() {
				eventNameReads++;
				return `event-${index}`;
			},
		} satisfies Misskey.entities.EmojiSimple));
		customEmojis.value = Array.from({ length: 100 }, (_, index) => emoji(`catalog-${index}`, `/catalog-${index}.png`));

		updateCustomEmojis(events);

		expect(eventNameReads).toBeLessThanOrEqual(events.length * 2);
	});
});

describe('removeCustomEmojis', () => {
	test('empty events preserve catalog order and item references', () => {
		const catalog = [emoji('first', '/first.png'), emoji('second', '/second.png')];
		customEmojis.value = catalog;

		removeCustomEmojis([]);

		expect(customEmojis.value).toHaveLength(2);
		expect(customEmojis.value[0]).toBe(catalog[0]);
		expect(customEmojis.value[1]).toBe(catalog[1]);
	});

	test('one matching name is removed and an unmatched event keeps the remaining entry', () => {
		const removed = emoji('removed', '/removed.png');
		const unchanged = emoji('unchanged', '/unchanged.png');
		customEmojis.value = [removed, unchanged];

		removeCustomEmojis([emoji('removed', '/event.png'), emoji('not-in-catalog', '/extra.png')]);

		expect(customEmojis.value).toHaveLength(1);
		expect(customEmojis.value[0]).toBe(unchanged);
	});

	test('a batch removes every catalog duplicate and preserves the order of unmatched entries', () => {
		const first = emoji('first', '/first.png');
		const second = emoji('second', '/second.png');
		const duplicateFirst = emoji('first', '/duplicate-first.png');
		const third = emoji('third', '/third.png');
		const duplicateSecond = emoji('second', '/duplicate-second.png');
		customEmojis.value = [first, second, duplicateFirst, third, duplicateSecond];

		removeCustomEmojis([emoji('first', '/event-first.png'), emoji('second', '/event-second.png')]);

		expect(customEmojis.value).toHaveLength(1);
		expect(customEmojis.value[0]).toBe(third);
	});

	test('event names are indexed instead of reread for every catalog entry', () => {
		let eventNameReads = 0;
		const events = Array.from({ length: 20 }, (_, index) => ({
			aliases: [],
			category: null,
			url: `/event-${index}.png`,
			get name() {
				eventNameReads++;
				return `event-${index}`;
			},
		} satisfies Misskey.entities.EmojiSimple));
		customEmojis.value = Array.from({ length: 100 }, (_, index) => emoji(`catalog-${index}`, `/catalog-${index}.png`));

		removeCustomEmojis(events);

		expect(eventNameReads).toBeLessThanOrEqual(events.length * 2);
	});
});
