/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test, vi } from 'vitest';

// テーマの解析 (json5) の chunk を読めなかったときは、テーマの不正ではなく読み込みの失敗として出す (#3613)。
const h = vi.hoisted(() => ({ alert: vi.fn() }));
vi.mock('@@/js/theme-code.js', async () => {
	throw new Error('chunk load failed');
});
vi.mock('@/os.js', () => ({ alert: h.alert }));

import { installTheme, handleThemeInstallError, ThemeCodeLoadError } from '@/theme.js';

describe('installTheme when the parser cannot be loaded', () => {
	test('rejects with ThemeCodeLoadError, reported as a load failure', async () => {
		vi.spyOn(console, 'error').mockImplementation(() => {});
		const err = await installTheme('{ id: "x", name: "X", base: "dark", props: {} }').catch(e => e);
		expect(err).toBeInstanceOf(ThemeCodeLoadError);

		handleThemeInstallError(err);
		expect(h.alert).toHaveBeenCalledWith({ type: 'error', title: expect.any(String), text: 'CODE: ASYNC_COMP_LOAD_FAIL' });
	});

	test('an invalid theme is still reported as invalid', () => {
		h.alert.mockClear();
		vi.spyOn(console, 'error').mockImplementation(() => {});
		handleThemeInstallError(new Error('This theme is invaild'));
		expect(h.alert).toHaveBeenCalledTimes(1);
		expect(h.alert.mock.calls[0][0].text).not.toBe('CODE: ASYNC_COMP_LOAD_FAIL');
	});
});
