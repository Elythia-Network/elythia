/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, describe, expect, test, vi } from 'vitest';
import { defineComponent, h, nextTick, reactive } from 'vue';
import type { PropType } from 'vue';
import { cleanup, fireEvent, render } from '@testing-library/vue';
import MkSensitiveMedia from '@/components/MkSensitiveMedia.vue';
import { _setSensitiveMediaImporterForTest, launchServerPlugins } from '@/plugin-api.js';
import type { PluginHost } from '@/plugin-api.js';

// 本番の PreferencesManager と同じ形にする。s は素の object で、変化を伝えるのは r の ref だけ。
// s を reactive にすると、s を読む実装でもテストが通ってしまう。
const prefs = vi.hoisted(() => ({
	s: {} as Record<string, unknown>,
	r: {} as Record<string, { value: unknown }>,
}));
vi.mock('@/preferences.js', async () => {
	const { ref } = await import('vue');
	for (const [key, value] of Object.entries(defaults())) {
		prefs.s[key] = value;
		prefs.r[key] = ref(value);
	}
	return { prefer: { s: prefs.s, r: prefs.r } };
});

function defaults(): Record<string, unknown> {
	return {
		nsfw: 'respect',
		dataSaver: { media: false, avatar: false, urlPreview: false, code: false },
		confirmWhenRevealingSensitiveMedia: false,
		highlightSensitiveMedia: false,
	};
}

// rewriteRawState と同じく、s を書いてから r を書く。
function setPref(key: string, value: unknown): void {
	prefs.s[key] = value;
	prefs.r[key].value = value;
}

function setDataSaverMedia(media: boolean): void {
	setPref('dataSaver', { ...(prefs.s.dataSaver as object), media });
}

const confirm = vi.hoisted(() => vi.fn(async () => ({ canceled: false })));
vi.mock('@/os.js', () => ({ confirm }));

afterEach(() => {
	cleanup();
	for (const [key, value] of Object.entries(defaults())) setPref(key, value);
	confirm.mockReset();
	confirm.mockImplementation(async () => ({ canceled: false }));
	_setSensitiveMediaImporterForTest(null);
	vi.restoreAllMocks();
});

type MediaProps = { isSensitive: boolean; type?: 'image' | 'video' | 'audio'; size?: number | null; controls?: boolean };

function mount(props: MediaProps, onOuterClick?: () => void) {
	const Wrapper = defineComponent({
		props: { media: { type: Object as PropType<MediaProps>, required: true } },
		setup(p) {
			return () => h('div', { 'data-testid': 'outer', onClick: onOuterClick }, [
				h(MkSensitiveMedia, p.media, {
					default: ({ hide }: { hide: () => void }) => h('img', { 'data-testid': 'content', src: 'https://remote.example/a.png', onClick: hide }),
				}),
			]);
		},
	});
	const media = reactive<MediaProps>({ ...props });
	return { view: render(Wrapper, { props: { media } }), media };
}

function hiddenButton(view: ReturnType<typeof render>): HTMLElement | null {
	return view.queryByTestId('sensitive-media-hidden');
}

describe('MkSensitiveMedia', () => {
	test('does not render the content while sensitive media is hidden', async () => {
		const { view } = mount({ isSensitive: true });
		expect(hiddenButton(view)).not.toBeNull();
		expect(view.queryByTestId('content')).toBeNull();

		await fireEvent.click(hiddenButton(view)!);
		await nextTick();
		expect(hiddenButton(view)).toBeNull();
		expect(view.queryByTestId('content')).not.toBeNull();
		expect(confirm).not.toHaveBeenCalled();
	});

	test('shows non-sensitive media immediately unless data saver hides it', async () => {
		const shown = mount({ isSensitive: false });
		expect(shown.view.queryByTestId('content')).not.toBeNull();
		cleanup();

		setDataSaverMedia(true);
		setPref('confirmWhenRevealingSensitiveMedia', true);
		const { view } = mount({ isSensitive: false, size: 2048 });
		expect(view.queryByTestId('content')).toBeNull();
		expect(hiddenButton(view)!.textContent).toContain('2KB');

		await fireEvent.click(hiddenButton(view)!);
		await nextTick();
		expect(view.queryByTestId('content')).not.toBeNull();
		expect(confirm).not.toHaveBeenCalled();
	});

	test('follows the force and ignore settings', () => {
		setPref('nsfw', 'force');
		const forced = mount({ isSensitive: false });
		expect(forced.view.queryByTestId('content')).toBeNull();
		cleanup();

		setPref('nsfw', 'ignore');
		const ignored = mount({ isSensitive: true });
		expect(ignored.view.queryByTestId('content')).not.toBeNull();
	});

	test('keeps the media hidden when the reveal confirmation is canceled', async () => {
		setPref('confirmWhenRevealingSensitiveMedia', true);
		confirm.mockImplementation(async () => ({ canceled: true }));
		const { view } = mount({ isSensitive: true });

		await fireEvent.click(hiddenButton(view)!);
		await nextTick();
		expect(confirm).toHaveBeenCalledTimes(1);
		expect(view.queryByTestId('content')).toBeNull();

		confirm.mockImplementation(async () => ({ canceled: false }));
		await fireEvent.click(hiddenButton(view)!);
		await nextTick();
		expect(view.queryByTestId('content')).not.toBeNull();
	});

	test('does not reveal with a confirmation started before the settings changed', async () => {
		setPref('confirmWhenRevealingSensitiveMedia', true);
		let answer: (value: { canceled: boolean }) => void = () => {};
		confirm.mockImplementation(() => new Promise(resolve => { answer = resolve; }));
		const { view } = mount({ isSensitive: true });

		await fireEvent.click(hiddenButton(view)!);
		await fireEvent.click(hiddenButton(view)!);
		expect(confirm).toHaveBeenCalledTimes(1);

		setPref('nsfw', 'force');
		await nextTick();
		answer({ canceled: false });
		await new Promise(resolve => setTimeout(resolve, 0));
		await nextTick();
		expect(view.queryByTestId('content')).toBeNull();

		await fireEvent.click(hiddenButton(view)!);
		expect(confirm).toHaveBeenCalledTimes(2);
		answer({ canceled: false });
		await new Promise(resolve => setTimeout(resolve, 0));
		await nextTick();
		expect(view.queryByTestId('content')).not.toBeNull();
	});

	test('hides again when the sensitivity or the settings change', async () => {
		const { view, media } = mount({ isSensitive: false });
		expect(view.queryByTestId('content')).not.toBeNull();

		media.isSensitive = true;
		await nextTick();
		expect(view.queryByTestId('content')).toBeNull();

		await fireEvent.click(hiddenButton(view)!);
		await nextTick();
		expect(view.queryByTestId('content')).not.toBeNull();

		setDataSaverMedia(true);
		await nextTick();
		expect(view.queryByTestId('content')).toBeNull();

		await fireEvent.click(hiddenButton(view)!);
		await nextTick();
		setPref('nsfw', 'force');
		await nextTick();
		expect(view.queryByTestId('content')).toBeNull();
	});

	test('can be hidden again from the button and from the slot', async () => {
		const { view } = mount({ isSensitive: true });
		await fireEvent.click(hiddenButton(view)!);
		await nextTick();

		await fireEvent.click(view.getByTestId('sensitive-media-hide'));
		expect(view.queryByTestId('content')).toBeNull();

		await fireEvent.click(hiddenButton(view)!);
		await nextTick();
		await fireEvent.click(view.getByTestId('content'));
		expect(view.queryByTestId('content')).toBeNull();
	});

	test('omits the hide button without controls', async () => {
		const { view } = mount({ isSensitive: false, controls: false });
		expect(view.queryByTestId('content')).not.toBeNull();
		expect(view.queryByTestId('sensitive-media-hide')).toBeNull();
	});

	test('does not let the reveal click reach the surrounding card', async () => {
		const outer = vi.fn();
		const { view } = mount({ isSensitive: true }, outer);
		await fireEvent.click(hiddenButton(view)!);
		await nextTick();
		expect(view.queryByTestId('content')).not.toBeNull();
		await fireEvent.click(view.getByTestId('sensitive-media-hide'));
		expect(outer).not.toHaveBeenCalled();
	});

	test('labels the hidden media by its type', () => {
		setDataSaverMedia(true);
		const video = mount({ isSensitive: true, type: 'video', size: 1024 });
		expect(hiddenButton(video.view)!.textContent).toContain('Video');
		cleanup();
		const audio = mount({ isSensitive: false, type: 'audio' });
		expect(hiddenButton(audio.view)!.querySelector('.ti-music')).not.toBeNull();
	});

	test('outlines sensitive media when the user asks for it', () => {
		setPref('highlightSensitiveMedia', true);
		const sensitive = mount({ isSensitive: true });
		const root = sensitive.view.getByTestId('sensitive-media');
		cleanup();
		const plain = mount({ isSensitive: false });
		expect(root.className).not.toBe(plain.view.getByTestId('sensitive-media').className);
	});
});

describe('plugin host sensitive media helpers', () => {
	async function host(): Promise<PluginHost> {
		let captured: PluginHost | undefined;
		await launchServerPlugins([{ name: 'example', setup(value) { captured = value; } }]);
		return captured!;
	}

	test('reports the default visibility under the user settings', async () => {
		const h = await host();
		expect(h.shouldHideSensitiveMedia(true)).toBe(true);
		expect(h.shouldHideSensitiveMedia(false)).toBe(false);

		setDataSaverMedia(true);
		expect(h.shouldHideSensitiveMedia(false)).toBe(true);
		setDataSaverMedia(false);
		setPref('nsfw', 'ignore');
		expect(h.shouldHideSensitiveMedia(true)).toBe(false);
	});

	test('asks before revealing only when the user wants confirmation', async () => {
		const h = await host();
		expect(await h.canRevealSensitiveMedia(true)).toBe(true);
		expect(confirm).not.toHaveBeenCalled();

		setPref('confirmWhenRevealingSensitiveMedia', true);
		confirm.mockImplementation(async () => ({ canceled: true }));
		expect(await h.canRevealSensitiveMedia(false)).toBe(true);
		expect(confirm).not.toHaveBeenCalled();
		expect(await h.canRevealSensitiveMedia(true)).toBe(false);
		expect(confirm).toHaveBeenCalledTimes(1);
	});

	test('still launches every plugin and keeps media hidden when the settings module cannot load', async () => {
		const error = vi.spyOn(console, 'error').mockImplementation(() => {});
		_setSensitiveMediaImporterForTest(() => Promise.reject(new Error('chunk load failed')));
		const hosts: PluginHost[] = [];
		await launchServerPlugins([
			{ name: 'first', setup(value) { hosts.push(value); } },
			{ name: 'second', setup(value) { hosts.push(value); } },
		]);

		expect(hosts.map(value => value.name)).toEqual(['first', 'second']);
		expect(error).toHaveBeenCalled();
		for (const value of hosts) {
			expect(value.shouldHideSensitiveMedia(false)).toBe(true);
			expect(await value.canRevealSensitiveMedia(true)).toBe(false);
			expect(await value.canRevealSensitiveMedia(false)).toBe(true);
		}
		expect(confirm).not.toHaveBeenCalled();
	});
});
