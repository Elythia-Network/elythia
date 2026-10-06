/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, assert, describe, test } from 'vitest';
import { cleanup, render } from '@testing-library/vue';
import { nextTick } from 'vue';
import { directives } from '@/directives/index.js';
import { components } from '@/components/index.js';
import XBackdrop from '@/pages/welcome.elythia-backdrop.vue';
import { instance } from '@/instance.js';
import { entranceThemeVars } from '@/utility/elythia-entrance.js';

describe('entranceThemeVars', () => {
	test('uses translucent glass panels when blur is allowed', () => {
		const vars = entranceThemeVars(true);
		assert.strictEqual(vars['--MI_THEME-panel'], 'rgba(18, 22, 56, 0.55)');
		assert.include(vars['--ELYTHIA-panelBackdrop'], 'blur(');
		assert.strictEqual(vars['color-scheme'], 'dark');
	});

	// ぼかしを切っている人に薄いパネルを出すと、夜空の絵が透けて文字が読みにくい
	test('falls back to nearly opaque panels without blur', () => {
		const vars = entranceThemeVars(false);
		assert.strictEqual(vars['--MI_THEME-panel'], 'rgba(18, 22, 56, 0.92)');
		assert.strictEqual(vars['--ELYTHIA-panelBackdrop'], 'none');
	});
});

describe('welcome.elythia-backdrop', () => {
	const original = instance.backgroundImageUrl;

	afterEach(() => {
		instance.backgroundImageUrl = original;
		cleanup();
	});

	const artOf = (container: Element) => Array.from(container.querySelectorAll<HTMLElement>('div'))
		.find(el => el.style.backgroundImage.includes('elythia-entrance-art'));

	test('draws the night sky with the icon art when no background image is set', async () => {
		instance.backgroundImageUrl = null;
		const { container } = render(XBackdrop, { global: { directives, components } });
		await nextTick();
		assert.exists(artOf(container), 'icon art layer exists');
		assert.exists(container.querySelector('svg'), 'sparkles exist');
	});

	// ログインしていない人の画面の左パネルでは、画面全体でなくパネルの中に敷く
	test('fills only its container when contained', async () => {
		instance.backgroundImageUrl = null;
		const full = render(XBackdrop, { global: { directives, components } });
		await nextTick();
		const fullClass = (full.container.firstElementChild as HTMLElement).className;
		cleanup();
		const contained = render(XBackdrop, { props: { contained: true }, global: { directives, components } });
		await nextTick();
		const containedClass = (contained.container.firstElementChild as HTMLElement).className;
		assert.notInclude(fullClass, 'contained');
		assert.include(containedClass, 'contained');
	});

	// 管理者が背景画像を設定しているなら、その画像を出して夜空の絵は重ねない
	test('shows the configured background image instead of the night sky', async () => {
		instance.backgroundImageUrl = 'https://example.com/bg.jpg';
		const { container } = render(XBackdrop, { global: { directives, components } });
		await nextTick();
		assert.notExists(artOf(container), 'icon art layer is not drawn');
		const photo = Array.from(container.querySelectorAll<HTMLElement>('div'))
			.find(el => el.style.backgroundImage.includes('https://example.com/bg.jpg'));
		assert.exists(photo, 'configured background image is shown');
	});
});
