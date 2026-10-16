/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// `test/setup-theme.exclusions.ts` に載せたファイルでは、setup がテーマを当てず、
// `vi.resetModules()` のたびに `@/theme.js` が作り直されることを固定する (#3369)。
// 本家の `theme.test.ts` はこの前提で書かれている。このファイル自身も除外に載せて
// あり、同じ経路を通る。

import { describe, expect, test, vi } from 'vitest';

// 各テストの timeout。`@/theme.js` を読み直すたびに依存ごと評価し直すので、5 秒の
// 既定では足りないことがある。
const RELOAD_TIMEOUT_MS = 60000;

const loadThemeModule = async () => {
	vi.resetModules();
	return await import('@/theme.js');
};

describe('files excluded from the test theme setup', () => {
	test('get a fresh ThemeManager after vi.resetModules()', async () => {
		const first = await loadThemeModule();
		const second = await loadThemeModule();
		expect(second.themeManager).not.toBe(first.themeManager);
	}, RELOAD_TIMEOUT_MS);

	test('start without a theme applied', async () => {
		const { themeManager } = await loadThemeModule();
		expect(themeManager.currentCompiledTheme).toBeNull();
		expect(themeManager.currentTheme).toBeNull();
	}, RELOAD_TIMEOUT_MS);
});
