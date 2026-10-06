/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { assert, describe, test } from 'vitest';
import tinycolor from 'tinycolor2';
import baseLight from '@@/themes/_light.json5';
import baseDark from '@@/themes/_dark.json5';
import lightTheme from '@@/themes/l-elythia.json5';
import darkTheme from '@@/themes/d-elythia.json5';
import { compile, getBuiltinThemes } from '@@/js/theme.js';
import type { Theme } from '@@/js/theme.js';

// 既定のテーマは誰でも目にするので、配色を変えたときに読めなくならないことをここで守る。
// 基準は WCAG AA (本文 4.5:1)
const contrast = (a: string, b: string) => tinycolor.readability(a, b);

describe.each([
	['Elythia Light', lightTheme as Theme],
	['Elythia Dark', darkTheme as Theme],
])('%s', (_name, theme) => {
	// 画面に当たるのは、ベースのテーマの値を重ねてから解決した色 (src/theme.ts と同じ手順)
	const base = [baseLight, baseDark].find(x => x.id === theme.base) as Theme;
	const c = compile({ ...theme, props: { ...base.props, ...theme.props } });

	test('body text is readable on panels and the background', () => {
		assert.isAtLeast(contrast(c.fg, c.panel), 4.5);
		assert.isAtLeast(contrast(c.fg, c.bg), 4.5);
	});

	test('text on accent-colored buttons is readable', () => {
		assert.isAtLeast(contrast(c.fgOnAccent, c.accent), 4.5);
	});

	test('links, mentions, hashtags and renote labels are readable on panels', () => {
		assert.isAtLeast(contrast(c.link, c.panel), 4.5);
		assert.isAtLeast(contrast(c.mention, c.panel), 4.5);
		assert.isAtLeast(contrast(c.hashtag, c.panel), 4.5);
		assert.isAtLeast(contrast(c.renote, c.panel), 4.5);
	});

	// グラデーションのボタンは文字が中央に来るが、両端でも基準を割らないようにしておく
	test('text on gradient buttons is readable at both ends', () => {
		assert.isAtLeast(contrast(c.fgOnAccent, c.buttonGradateA), 4.5);
		assert.isAtLeast(contrast(c.fgOnAccent, c.buttonGradateB), 4.5);
	});

	test('is offered as a built-in theme', async () => {
		const builtins = await getBuiltinThemes();
		assert.include(builtins.map(t => t.id), theme.id);
	});
});
