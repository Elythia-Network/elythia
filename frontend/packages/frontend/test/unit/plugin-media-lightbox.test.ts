/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, describe, expect, test } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/vue';
import MkLightboxItem from '@/components/MkLightbox.item.vue';
import { preferState } from '../setup.unit.js';

describe('plugin media in the native lightbox item', () => {
	afterEach(() => {
		cleanup();
		delete preferState.nsfw;
	});

	test('uses plugin alt text and starts sensitive content hidden until revealed', async () => {
		preferState.nsfw = 'respect';
		const view = render(MkLightboxItem, {
			props: {
				content: { id: 'plugin-image', type: 'image', url: 'blob:plugin-image', alt: 'Plugin image alt', isSensitive: true },
				activated: true,
				pixelatedZoom: false,
			},
			global: { stubs: { MkCondensedLine: true, MkLoading: true } },
		});

		expect(view.container.querySelector('[data-gallery-click-action="hidden"]')).not.toBeNull();
		await fireEvent.click(view.container.querySelector('[data-gallery-click-action="hidden"]')!);
		expect(view.container.querySelector('[data-gallery-click-action="hidden"]')).toBeNull();
		expect(view.container.querySelector('img[alt="Plugin image alt"]')).not.toBeNull();
	});

	test('honors initiallyRevealed while retaining the configured NSFW default', () => {
		preferState.nsfw = 'respect';
		const revealed = render(MkLightboxItem, {
			props: {
				content: { id: 'plugin-image', type: 'image', url: 'blob:plugin-image', alt: 'Plugin image alt', isSensitive: true },
				activated: true,
				initiallyRevealed: true,
				pixelatedZoom: false,
			},
			global: { stubs: { MkCondensedLine: true, MkLoading: true } },
		});
		expect(revealed.container.querySelector('[data-gallery-click-action="hidden"]')).toBeNull();
		expect(revealed.container.querySelector('img[alt="Plugin image alt"]')).not.toBeNull();
		revealed.unmount();

		preferState.nsfw = 'force';
		const forced = render(MkLightboxItem, {
			props: {
				content: { id: 'plugin-image', type: 'image', url: 'blob:plugin-image', alt: 'Plugin image alt', isSensitive: false },
				activated: true,
				pixelatedZoom: false,
			},
			global: { stubs: { MkCondensedLine: true, MkLoading: true } },
		});
		expect(forced.container.querySelector('[data-gallery-click-action="hidden"]')).not.toBeNull();
	});

	test('keeps native DriveFile sensitivity and comment behavior', async () => {
		preferState.nsfw = 'respect';
		const view = render(MkLightboxItem, {
			props: {
				content: {
					id: 'drive-image',
					type: 'image',
					url: 'https://example.test/drive-image.png',
					alt: 'Fallback alt',
					isSensitive: false,
					file: { isSensitive: true, comment: 'Drive file comment' } as NonNullable<import('@/components/MkLightbox.item.vue').Content['file']>,
				},
				activated: true,
				pixelatedZoom: false,
			},
			global: { stubs: { MkCondensedLine: true, MkLoading: true } },
		});

		expect(view.container.querySelector('[data-gallery-click-action="hidden"]')).not.toBeNull();
		await fireEvent.click(view.container.querySelector('[data-gallery-click-action="hidden"]')!);
		expect(view.container.querySelector('img[alt="Drive file comment"]')).not.toBeNull();
	});
});
