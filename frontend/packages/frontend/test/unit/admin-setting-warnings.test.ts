/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, beforeEach, vi } from 'vitest';

const h = vi.hoisted(() => ({ api: vi.fn() }));
vi.mock('@/utility/misskey-api.js', () => ({
	misskeyApi: (...args: unknown[]) => h.api(...args),
}));

import {
	SETTING_WARNING_IDS,
	classifySettingWarnings,
	lacksBotProtection,
	loadDismissedWarnings,
	parseDismissedWarnings,
	updateDismissedWarnings,
} from '@/utility/admin-setting-warnings.js';

const allActive = { maintainer: true, inquiryUrl: true, botProtection: true, emailServer: true };

describe('admin setting warnings (#3190)', () => {
	beforeEach(() => {
		h.api.mockReset();
	});

	test('shows every active warning when nothing is hidden', () => {
		expect(classifySettingWarnings({ active: allActive, singleUserMode: false, dismissed: [] }))
			.toEqual({ visible: [...SETTING_WARNING_IDS], hidden: [] });
	});

	test('hidden warnings are counted separately, not shown', () => {
		expect(classifySettingWarnings({ active: allActive, singleUserMode: false, dismissed: ['inquiryUrl', 'emailServer'] }))
			.toEqual({ visible: ['maintainer', 'botProtection'], hidden: ['inquiryUrl', 'emailServer'] });
	});

	test('warnings whose condition does not hold are neither shown nor counted', () => {
		expect(classifySettingWarnings({
			active: { maintainer: false, inquiryUrl: true, botProtection: false, emailServer: false },
			singleUserMode: false,
			dismissed: ['maintainer'],
		})).toEqual({ visible: ['inquiryUrl'], hidden: [] });
	});

	// お一人様モードでは再表示しても出ないので、「非表示にした」数にも入れない。
	test('single user mode shows none of them and hides nothing', () => {
		expect(classifySettingWarnings({ active: allActive, singleUserMode: true, dismissed: ['maintainer'] }))
			.toEqual({ visible: [], hidden: [] });
	});

	test('the unresolved abuse report warning is not dismissable', () => {
		expect(SETTING_WARNING_IDS).not.toContain('abuseReport');
		expect([...SETTING_WARNING_IDS].sort()).toEqual(['botProtection', 'emailServer', 'inquiryUrl', 'maintainer']);
	});

	test('the stored value is not trusted', () => {
		expect(parseDismissedWarnings(null)).toEqual([]);
		expect(parseDismissedWarnings('maintainer')).toEqual([]);
		expect(parseDismissedWarnings(['maintainer', 'unknown', 1, 'maintainer', 'emailServer'])).toEqual(['maintainer', 'emailServer']);
	});

	describe('bot protection', () => {
		const open = {
			disableRegistration: false,
			enableHcaptcha: false,
			enableRecaptcha: false,
			enableTurnstile: false,
			enableMcaptcha: false,
		};

		test('open registration without captcha lacks protection', () => {
			expect(lacksBotProtection(open)).toBe(true);
		});

		// 承認制はそれ自体が登録のゲート (#2557)。
		test('approval-required signup is protected', () => {
			expect(lacksBotProtection({ ...open, approvalRequiredForSignup: true })).toBe(false);
		});

		test('invite-only and captcha are protected, as upstream', () => {
			expect(lacksBotProtection({ ...open, disableRegistration: true })).toBe(false);
			for (const key of ['enableHcaptcha', 'enableRecaptcha', 'enableTurnstile', 'enableMcaptcha'] as const) {
				expect(lacksBotProtection({ ...open, [key]: true })).toBe(false);
			}
		});
	});

	describe('registry', () => {
		const target = { scope: ['mkgo', 'adminPanel'], key: 'dismissedSettingWarnings' };

		test('loads the hidden warnings of the account', async () => {
			h.api.mockResolvedValue(['botProtection', 'bogus']);
			expect(await loadDismissedWarnings()).toEqual(['botProtection']);
			expect(h.api).toHaveBeenCalledWith('i/registry/get', target);
		});

		test('a missing key means nothing is hidden', async () => {
			h.api.mockRejectedValue({ code: 'NO_SUCH_KEY' });
			expect(await loadDismissedWarnings()).toEqual([]);
		});

		// 障害を「何も隠していない」にすると、次の保存で既存の非表示を消す。
		test('an unreadable key is reported, not treated as empty', async () => {
			h.api.mockRejectedValue({ code: 'INTERNAL_ERROR' });
			expect(await loadDismissedWarnings()).toBeNull();
		});

		// 別の端末で閉じた分を上書きで消さない。
		test('an update re-reads the stored value before writing', async () => {
			h.api.mockImplementation(async (endpoint: string) => endpoint === 'i/registry/get' ? ['maintainer'] : undefined);
			expect(await updateDismissedWarnings(current => [...current, 'emailServer'])).toEqual(['maintainer', 'emailServer']);
			expect(h.api).toHaveBeenLastCalledWith('i/registry/set', { ...target, value: ['maintainer', 'emailServer'] });
		});

		test('an update does not write when the stored value cannot be read', async () => {
			h.api.mockRejectedValue({ code: 'INTERNAL_ERROR' });
			// op は current を読まない形にする。読むと null で TypeError になり、ガードが無くても落ちる。
			await expect(updateDismissedWarnings(() => ['emailServer'])).rejects.toThrow();
			expect(h.api).not.toHaveBeenCalledWith('i/registry/set', expect.anything());
		});

		// 連続して閉じたとき、後の保存が前の保存の結果を読んでから書く。
		test('updates run one at a time', async () => {
			let stored: unknown = [];
			const log: string[] = [];
			h.api.mockImplementation(async (endpoint: string, params: { value?: unknown }) => {
				log.push(endpoint);
				await new Promise(resolve => setTimeout(resolve, 5));
				if (endpoint === 'i/registry/get') return stored;
				stored = params.value;
				return undefined;
			});
			const first = updateDismissedWarnings(current => [...current, 'maintainer']);
			const second = updateDismissedWarnings(current => [...current, 'emailServer']);
			await Promise.all([first, second]);
			expect(stored).toEqual(['maintainer', 'emailServer']);
			expect(log).toEqual(['i/registry/get', 'i/registry/set', 'i/registry/get', 'i/registry/set']);
		});

		test('a failed update does not block the next one', async () => {
			h.api.mockRejectedValueOnce({ code: 'INTERNAL_ERROR' });
			await expect(updateDismissedWarnings(current => current)).rejects.toThrow();
			h.api.mockImplementation(async (endpoint: string) => endpoint === 'i/registry/get' ? [] : undefined);
			expect(await updateDismissedWarnings(current => [...current, 'inquiryUrl'])).toEqual(['inquiryUrl']);
		});
	});
});
