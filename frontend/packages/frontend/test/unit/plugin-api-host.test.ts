/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import {
	_resetPasteTransformsForTest,
	_resetPreviewsForTest,
	launchServerPlugins,
} from '@/plugin-api.js';
import type { PluginHost } from '@/plugin-api.js';

const popupCapture = vi.hoisted(() => ({ props: undefined as unknown, events: undefined as unknown }));

vi.mock('@/i.js', async importOriginal => {
	const actual = await importOriginal<typeof import('@/i.js')>();
	return { ...actual, $i: { id: 'test-user', token: 'session-token', policies: {} } };
});
vi.mock('@/utility/misskey-api.js', async importOriginal => {
	const actual = await importOriginal<typeof import('@/utility/misskey-api.js')>();
	return { ...actual, misskeyApi: vi.fn(async () => []) };
});
vi.mock('@/os.js', () => ({
	popupAsyncWithDialog: async (_component: unknown, props: unknown, events: unknown) => {
		popupCapture.props = props;
		popupCapture.events = events;
		return { dispose: vi.fn() };
	},
}));
vi.mock('@/components/MkLightbox.vue', () => ({ default: {} }));

let originalWindowFetch: typeof window.fetch;

beforeEach(() => {
	originalWindowFetch = window.fetch;
	window.fetch = globalThis.fetch;
});

afterEach(() => {
	_resetPasteTransformsForTest();
	_resetPreviewsForTest();
	fetchMock.resetMocks();
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
	window.fetch = originalWindowFetch;
});

describe('plugin host fetch', () => {
	test('uses the session only for this plugin root-relative path', async () => {
		const fetchSpy = vi.spyOn(window, 'fetch').mockResolvedValue(new Response('{}'));
		let host!: PluginHost;
		await launchServerPlugins([{ name: 'example', setup(value) { host = value; } }]);

		await host.fetch('/api/plugin/example/media/42?size=small', { method: 'GET' });

		const [request, init] = fetchSpy.mock.calls[0];
		expect(fetchSpy).toHaveBeenCalledTimes(1);
		expect(new URL(String(request), window.location.href).pathname).toBe('/api/plugin/example/media/42');
		expect(init?.mode).toBe('same-origin');
		expect(init?.credentials).toBe('omit');
		expect(init?.redirect).toBe('error');
		expect(new Headers(init?.headers).get('Authorization')).toBe('Bearer session-token');
	});

	test.each([
		'https://elsewhere.example/path',
		'/api/plugin/another-plugin/private',
		'/api/plugin/example/../other',
		'/api/plugin/example/media/42?i=caller-token',
	])('rejects a fetch path outside the plugin API boundary: %s', async path => {
		const fetchSpy = vi.spyOn(window, 'fetch').mockResolvedValue(new Response('{}'));
		let host!: PluginHost;
		await launchServerPlugins([{ name: 'example', setup(value) { host = value; } }]);

		await expect(host.fetch(path)).rejects.toThrow();
		expect(fetchSpy).not.toHaveBeenCalled();
	});
});

describe('plugin host media viewer', () => {
	test.each([
		{ id: 'svg', type: 'image' as const, blob: new Blob(['<svg/>'], { type: 'image/svg+xml' }) },
		{ id: 'mismatch', type: 'image' as const, blob: new Blob(['audio'], { type: 'audio/mpeg' }) },
		{ id: 'unsafe', type: 'video' as const, blob: new Blob(['script'], { type: 'text/html' }) },
	])('rejects media that is not browser-safe for its declared type: $id', async item => {
		let host!: PluginHost;
		await launchServerPlugins([{ name: 'example', setup(value) { host = value; } }]);

		await expect(host.openMedia([item])).rejects.toThrow();
	});

	test('treats an empty media collection as a no-op', async () => {
		let host!: PluginHost;
		await launchServerPlugins([{ name: 'example', setup(value) { host = value; } }]);

		await expect(host.openMedia([])).resolves.toBeUndefined();
	});

	test('passes alt text and sensitive reveal state into the native lightbox lifecycle', async () => {
		const createObjectURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:plugin-media');
		const revokeObjectURL = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
		let host!: PluginHost;
		await launchServerPlugins([{ name: 'example', setup(value) { host = value; } }]);

		await host.openMedia([{
			id: 'sensitive-image',
			type: 'image',
			blob: new Blob(['image'], { type: 'image/png' }),
			alt: 'A sensitive image',
			isSensitive: true,
			initiallyRevealed: true,
		}], 'sensitive-image');

		const dialog = popupCapture.props as { contents: Array<Record<string, unknown>>; initiallyRevealedContentIds: string[] };
		expect(dialog.contents[0]).toMatchObject({ id: 'sensitive-image', alt: 'A sensitive image', isSensitive: true });
		expect(dialog.initiallyRevealedContentIds).toEqual(['sensitive-image']);
		expect(createObjectURL).toHaveBeenCalledTimes(1);
		const events = popupCapture.events as { closed: () => void };
		events.closed();
		expect(revokeObjectURL).toHaveBeenCalledWith('blob:plugin-media');
	});
});
