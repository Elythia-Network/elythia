/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// `test/setup-theme.unit.ts` が、`@/theme.js` を読んだテストファイルにテーマを当てて
// いることを固定する (#3369)。これが外れると、テーマを読む部品 (`v-panel`、
// `initChart`) を描くテストが非同期の読み込みの着地しだいで不定期に落ちる (#3366)
// ので、ここで決定的に落とす。

import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { afterEach, describe, expect, test } from 'vitest';
import { cleanup, render } from '@testing-library/vue';
import { defineComponent, h, withDirectives } from 'vue';
// **`@/theme.js` を読む import の中で最初に置く。** 読み終えた直後の状態を記録する。
import { themedAtImport } from '../theme-state-at-import.js';
import { themeManager } from '@/theme.js';
import { panelDirective } from '@/directives/panel.js';
import { initChart } from '@/utility/init-chart.js';
import { THEME_SETUP_EXCLUDED_FILES } from '../setup-theme.exclusions.js';

describe('unit test setup applies a theme', () => {
	afterEach(() => {
		cleanup();
	});

	test('a compiled theme is current as soon as @/theme.js is loaded', () => {
		expect(themedAtImport).toBe(true);
		expect(themeManager.currentCompiledTheme?.panel).toBeTruthy();
		expect(themeManager.currentCompiledTheme?.fg).toBeTruthy();
	});

	// `directives/panel.ts` の `mounted` は `currentCompiledTheme!.panel` を読む。
	// 本物の部品 (`MkDriveFileThumbnail` など) と同じく directive を通して描く。
	test('v-panel mounts without throwing', () => {
		const Panel = defineComponent({
			render: () => withDirectives(h('div', { 'data-testid': 'panel' }), [[panelDirective]]),
		});
		const { getByTestId } = render(Panel);
		expect(getByTestId('panel').style.backgroundColor).toMatch(/^var\(--MI_THEME-(bg|panel)\)$/);
	});

	test('initChart reads the theme without throwing', () => {
		expect(() => initChart()).not.toThrow();
	});
});

describe('test theme setup exclusions', () => {
	// 本家の `theme.test.ts` を外し忘れると、`vi.resetModules()` で新しい
	// `ThemeManager` を得る前提が黙って崩れる。
	test('upstream theme.test.ts is excluded', () => {
		expect(THEME_SETUP_EXCLUDED_FILES).toContain('theme.test.ts');
	});

	// 改名や削除で使われなくなった項目を残さない。
	test.each(THEME_SETUP_EXCLUDED_FILES.map((name) => [name]))('%s exists', (name) => {
		const testPath = expect.getState().testPath;
		expect(testPath).toBeTypeOf('string');
		expect(existsSync(join(dirname(testPath as string), name))).toBe(true);
	});
});
