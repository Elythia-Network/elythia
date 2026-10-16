/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test, vi } from 'vitest';

// 投稿フォームのダイアログの読み込みに失敗したら、エラーを出して post() は投げない (#3613)。
// ブラウザは取得に失敗したモジュールをページを読み込み直すまで覚えているので、
// エラーのダイアログで再読み込みを促す。
vi.mock('@/components/MkPostFormDialog.vue', async () => {
	throw new Error('chunk load failed');
});
vi.mock('@/components/MkPostForm.vue', () => ({ default: { name: 'FakePostForm' } }));
vi.mock('@/utility/please-login.js', () => ({ pleaseLogin: vi.fn(async () => true) }));
vi.mock('@/utility/show-moved-dialog.js', () => ({ showMovedDialog: vi.fn() }));

import * as os from '@/os.js';
import { i18n } from '@/i18n.js';
import MkDialog from '@/components/MkDialog.vue';

function errorDialogs() {
	return os.popups.value.filter(p => p.component === MkDialog && p.props.type === 'error');
}

describe('os.post when the dialog cannot be loaded', () => {
	test('shows an error dialog that offers a reload, and resolves', async () => {
		vi.spyOn(console, 'error').mockImplementation(() => {});
		const reload = vi.fn();
		const originalLocation = window.location;
		vi.stubGlobal('location', { ...originalLocation, reload });
		try {
			await expect(os.post()).resolves.toBeUndefined();
			const errors = errorDialogs();
			expect(errors).toHaveLength(1);
			expect(errors[0].props.text).toBe(`CODE: ASYNC_COMP_LOAD_FAIL\n${i18n.ts.reloadConfirm}`);
			expect(errors[0].props.showCancelButton).toBe(true);

			// キャンセルなら読み込み直さない
			errors[0].events.done({ canceled: true });
			await Promise.resolve(); await Promise.resolve();
			expect(reload).not.toHaveBeenCalled();
		} finally {
			vi.unstubAllGlobals();
		}
	});

	test('accepting the dialog reloads the page, and a later post() does not hang', async () => {
		vi.spyOn(console, 'error').mockImplementation(() => {});
		const reload = vi.fn();
		vi.stubGlobal('location', { ...window.location, reload });
		try {
			const before = errorDialogs().length;
			// 前の失敗の後でも post() は待ち続けずに終わる (失敗の後に postWhileLoading を戻す)
			await expect(os.post()).resolves.toBeUndefined();
			const errors = errorDialogs();
			expect(errors).toHaveLength(before + 1);
			errors.at(-1)!.events.done({ canceled: false });
			await vi.waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
		} finally {
			vi.unstubAllGlobals();
		}
	});
});
