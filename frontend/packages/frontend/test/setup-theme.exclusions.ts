/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Test files (relative to `test/unit/`) that must load the real `@/theme.js`
 * without the theme applied by `test/setup-theme.unit.ts`.
 */
export const THEME_SETUP_EXCLUDED_FILES: readonly string[] = [
	// 本家のテスト。`vi.resetModules()` で毎回新しい `ThemeManager` を得て、
	// テーマ未設定の状態から試す前提で書かれている。
	'theme.test.ts',
	// 上の除外が実際に効いていること (新しいインスタンス、テーマ未設定) を固定する。
	'theme-setup-excluded.test.ts',
];
