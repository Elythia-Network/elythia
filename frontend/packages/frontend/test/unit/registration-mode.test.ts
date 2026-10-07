/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect } from 'vitest';
import { isRegistrationClosedError, registrationModeOf, registrationModePatch } from '@/utility/registration-mode.js';
import type { RegistrationMode } from '@/utility/registration-mode.js';

describe('registration mode (#3186)', () => {
	test('derives the mode from meta', () => {
		expect(registrationModeOf({ closed: false, approval: false, disableRegistration: false })).toBe('open');
		expect(registrationModeOf({ closed: false, approval: false, disableRegistration: true })).toBe('invite');
		expect(registrationModeOf({ closed: false, approval: true, disableRegistration: false })).toBe('approval');
	});

	// 承認制の入口は招待制で、/api/signup は承認制で塞がり、どこからも登録できない。
	// 「承認制」と出すと動いていると読まれ、同じ選択肢は選び直せない。
	test('approval with invite-only matches no mode', () => {
		expect(registrationModeOf({ closed: false, approval: true, disableRegistration: true })).toBeNull();
	});

	// 閉じている間も承認制と招待制の値は立っている。先に見ないと「承認制」と出る。
	test('closed wins over the other values', () => {
		expect(registrationModeOf({ closed: true, approval: true, disableRegistration: true })).toBe('closed');
		expect(registrationModeOf({ closed: true, approval: false, disableRegistration: true })).toBe('closed');
		expect(registrationModeOf({ closed: true, approval: false, disableRegistration: false })).toBe('closed');
	});

	// 省略するとサーバー側の整合が既定を補い、選んだものと違う受け付け方になりうる。
	test('the open modes send all three values explicitly', () => {
		for (const mode of ['open', 'invite', 'approval'] as const) {
			expect(Object.keys(registrationModePatch[mode]).sort()).toEqual(['approvalRequiredForSignup', 'disableRegistration', 'registrationClosed']);
			expect(registrationModePatch[mode].registrationClosed).toBe(false);
		}
	});

	// 送った値をサーバーが整合させた結果が、選んだ受け付け方として読み戻せること。
	test('each patch reads back as the chosen mode', () => {
		for (const mode of ['open', 'invite', 'approval'] as const satisfies RegistrationMode[]) {
			const p = registrationModePatch[mode];
			expect(registrationModeOf({ closed: p.registrationClosed, approval: p.approvalRequiredForSignup === true, disableRegistration: p.disableRegistration === true })).toBe(mode);
		}
	});

	// 閉じている間も承認制を残す (申請者の照会を開けておく / 解除後に戻れる)。
	test('closing does not touch the approval setting', () => {
		expect(registrationModePatch.closed).toEqual({ registrationClosed: true });
	});

	test('recognizes the closed error only', () => {
		expect(isRegistrationClosedError({ code: 'REGISTRATION_CLOSED' })).toBe(true);
		expect(isRegistrationClosedError({ code: 'INVITATION_CODE_INVALID' })).toBe(false);
		expect(isRegistrationClosedError(null)).toBe(false);
		expect(isRegistrationClosedError(undefined)).toBe(false);
	});
});
