/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ライトボックスでメディアの読み込みに失敗したとき、読み込み中の表示を消して
// 失敗したことを出すことを固定する (#4150)。成功のイベントしか見ていないと、
// ブラウザが再生できない形式(ALACなど)で読み込み中の表示が出たままになる。

import { afterEach, describe, expect, test, vi } from 'vitest';
import { nextTick } from 'vue';
import { cleanup, fireEvent, render, type RenderResult } from '@testing-library/vue';
import { directives } from '@/directives/index.js';
import { components } from '@/components/index.js';
import type { MenuItem } from '@/types/menu.js';
import { prefer } from '@/preferences.js';
import { i18n } from '@/i18n.js';
import XItem from '@/components/MkLightbox.item.vue';
import type { Content } from '@/components/MkLightbox.item.vue';

// 「隠す」をメニューから押すために、メニューに渡された項目を受け取る。
// 他のos APIはそのまま使う
const popupMenu = vi.fn(async (_items: MenuItem[], _src?: unknown, _opts?: unknown) => ({ dispose: vi.fn() }));
vi.mock('@/os.js', async (importOriginal) => ({
	...await importOriginal<typeof import('@/os.js')>(),
	popupMenu: (items: MenuItem[], src?: unknown, opts?: unknown) => popupMenu(items, src, opts),
}));

// リンクに触れたときにダブルタップの判定へ渡らないことを見るため、判定に渡ったtouchstartを受け取る
const doubleTapTouchstart = vi.fn();
vi.mock('@/utility/double-tap.js', () => ({
	makeDoubleTapDetector: () => ({
		onTouchstart: (ev: TouchEvent) => doubleTapTouchstart(ev),
		onTouchmove: () => {},
	}),
}));

// 別オリジンにして、ビジュアライザがcrossorigin付きで読み始めるようにする
const MEDIA_URL = 'https://remote.example/files/media';

function renderItem(type: Content['type']): RenderResult {
	return render(XItem, {
		props: {
			content: { id: 'media', type, url: MEDIA_URL },
			activated: true,
			pixelatedZoom: false,
		},
		global: { directives, components },
	});
}

function mediaEl(item: RenderResult, tag: 'img' | 'video' | 'audio'): HTMLElement {
	const el = item.container.querySelector<HTMLElement>(`${tag}[src="${MEDIA_URL}"]`);
	if (el == null) throw new Error(`${tag} is not rendered`);
	return el;
}

function failureLink(item: RenderResult): HTMLElement {
	const link = item.getByTestId('lightbox-load-failed').querySelector('a');
	if (link == null) throw new Error('link is not rendered');
	return link;
}

function playIconCount(item: RenderResult): number {
	return item.container.querySelectorAll('.ti-player-play').length;
}

function expectFailed(item: RenderResult, failed: boolean): void {
	expect(item.queryByTestId('lightbox-load-failed') != null).toBe(failed);
	expect(item.queryByTestId('lightbox-loading') != null).toBe(!failed);
}

async function hideAndReveal(item: RenderResult): Promise<void> {
	await fireEvent.click(item.container.querySelector('.ti-dots') as HTMLElement);
	const hideItem = popupMenu.mock.calls[0][0].find((i) => 'text' in i && i.text === i18n.ts.hide);
	if (hideItem == null || !('action' in hideItem) || typeof hideItem.action !== 'function') throw new Error('hide menu item is not found');
	hideItem.action(new PointerEvent('click'));
	await nextTick();
	expect(item.queryByTestId('lightbox-load-failed')).toBeNull();

	const hidden = item.container.querySelector<HTMLElement>('[data-gallery-click-action="hidden"]');
	if (hidden == null) throw new Error('hidden placeholder is not rendered');
	await fireEvent.click(hidden);
	await vi.waitFor(() => mediaEl(item, 'img'));
}

afterEach(() => {
	cleanup();
	popupMenu.mockClear();
	doubleTapTouchstart.mockClear();
	vi.restoreAllMocks();
});

describe('lightbox load failure', () => {
	test('replaces the loading indicator with the failure when an image fails to load', async () => {
		const item = renderItem('image');
		expectFailed(item, false);

		await fireEvent.error(mediaEl(item, 'img'));

		expectFailed(item, true);
		expect(failureLink(item).getAttribute('href')).toBe(MEDIA_URL);
	});

	test('replaces the loading indicator with the failure when a video fails to load', async () => {
		const item = renderItem('video');

		await fireEvent.error(mediaEl(item, 'video'));

		expectFailed(item, true);
	});

	test('replaces the loading indicator with the failure when native audio fails to load', async () => {
		// テストの設定はtest/setup.unit.tsのモックで、ただのオブジェクト
		const settings = prefer.s as Record<string, unknown>;
		const original = settings.useNativeUiForVideoAudioPlayer;
		settings.useNativeUiForVideoAudioPlayer = true;
		try {
			const item = renderItem('audio');

			const audio = mediaEl(item, 'audio');
			await fireEvent.error(audio);

			expectFailed(item, true);
			// 使えない操作部が失敗の表示と重ならないように隠す
			expect(audio.parentElement?.style.display).toBe('none');
		} finally {
			settings.useNativeUiForVideoAudioPlayer = original;
		}
	});

	// ライトボックスはpointerdownでポインタを捕捉するので、リンクで止めないとクリックが
	// ライトボックスへ送られ、タブが開かずに閉じる (#4150のレビュー)
	test('does not capture the pointer when the failure link is pressed', async () => {
		const capture = vi.fn();
		vi.spyOn(HTMLElement.prototype, 'setPointerCapture').mockImplementation(capture);
		const item = renderItem('image');
		await fireEvent.error(mediaEl(item, 'img'));

		// 比較のため、メディアの側を押すと捕捉することを先に確かめる
		await fireEvent.pointerDown(mediaEl(item, 'img'), { pointerId: 1 });
		await fireEvent.pointerUp(mediaEl(item, 'img'), { pointerId: 1 });
		expect(capture).toHaveBeenCalledTimes(1);

		await fireEvent.pointerDown(failureLink(item), { pointerId: 2 });
		expect(capture).toHaveBeenCalledTimes(1);
	});

	test('does not close the lightbox when the failure link is clicked', async () => {
		const item = renderItem('image');
		await fireEvent.error(mediaEl(item, 'img'));
		// 比較のため、メディアの側を押して離すと閉じることを先に確かめる
		await fireEvent.pointerDown(mediaEl(item, 'img'), { pointerId: 1 });
		await fireEvent.pointerUp(mediaEl(item, 'img'), { pointerId: 1 });
		await fireEvent.click(mediaEl(item, 'img'));
		expect(item.emitted('close')).toHaveLength(1);

		// happy-domはリンクのクリックで実際に遷移して取りに行くので、遷移だけを止める
		const link = failureLink(item);
		link.addEventListener('click', (ev) => ev.preventDefault());
		await fireEvent.click(link);

		expect(item.emitted('close')).toHaveLength(1);
	});

	test('does not pass a touch on the failure link to the double tap detector', async () => {
		const item = renderItem('image');
		await fireEvent.error(mediaEl(item, 'img'));
		await fireEvent.touchStart(mediaEl(item, 'img'));
		expect(doubleTapTouchstart).toHaveBeenCalledTimes(1);

		await fireEvent.touchStart(failureLink(item));

		expect(doubleTapTouchstart).toHaveBeenCalledTimes(1);
	});

	test('hides the play icon and the control bar when a video fails to load', async () => {
		const item = renderItem('video');
		await vi.waitFor(() => {
			if (playIconCount(item) === 0) throw new Error('play icon is not rendered yet');
		});

		await fireEvent.error(mediaEl(item, 'video'));

		expect(playIconCount(item)).toBe(0);
	});

	test('does not carry the failure over after hiding and revealing again', async () => {
		const item = renderItem('image');
		await fireEvent.error(mediaEl(item, 'img'));
		expectFailed(item, true);

		await hideAndReveal(item);

		expectFailed(item, false);
	});

	test('ignores a late error from the element removed by hiding', async () => {
		const item = renderItem('image');
		const removed = mediaEl(item, 'img');

		await hideAndReveal(item);
		expect(removed.isConnected).toBe(false);
		await fireEvent.error(removed);

		expectFailed(item, false);
	});

	test('does not treat an error after loading as a load failure', async () => {
		const item = renderItem('image');

		await fireEvent.load(mediaEl(item, 'img'));
		await fireEvent.error(mediaEl(item, 'img'));

		expect(item.queryByTestId('lightbox-load-failed')).toBeNull();
	});

	test('shows the failure for audio only after the reload without CORS also fails', async () => {
		const item = renderItem('audio');
		const first = await vi.waitFor(() => mediaEl(item, 'audio'));
		expect(first.getAttribute('crossorigin')).toBe('anonymous');

		// 1回目の失敗はCORSの失敗かもしれないので、CORS無しで読み直す
		await fireEvent.error(first);
		const second = await vi.waitFor(() => {
			const el = mediaEl(item, 'audio');
			if (el === first) throw new Error('audio element is not recreated yet');
			return el;
		});
		expect(second.hasAttribute('crossorigin')).toBe(false);
		expectFailed(item, false);
		expect(playIconCount(item)).toBeGreaterThan(0);

		await fireEvent.error(second);

		expectFailed(item, true);
		expect(playIconCount(item)).toBe(0);
	});
});
