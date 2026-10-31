/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 登録フォームの利用者名とメールの確認が、入力を一度取り消した後も再び走ることを固定する (#3912)。
//
// throttle-debounce 5.x の引数なしの `cancel()` は debounce 関数を以後ずっと無効にする。
// 無効な名前や空欄を通った後に有効な値へ直しても問い合わせが飛ばず、「確認しています」の
// まま登録できなくなっていた。

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { cleanup, fireEvent, render, type RenderResult } from '@testing-library/vue';
import { directives } from '@/directives/index.js';
import { components } from '@/components/index.js';
import { instance } from '@/instance.js';
import { i18n } from '@/i18n.js';
import XSignupForm from '@/components/MkSignupDialog.form.vue';

// `vi.mock` の factory は巻き上げられるので、外側の変数は呼び出し時に解決する形で渡す。
const misskeyApi = vi.fn(async (endpoint: string, _data?: unknown, _token?: unknown, _signal?: AbortSignal): Promise<unknown> => {
	if (endpoint === 'username/available') return { available: true };
	if (endpoint === 'email-address/available') return { available: true, reason: null };
	throw new Error(`unexpected endpoint: ${endpoint}`);
});
vi.mock('@/utility/misskey-api.js', () => ({
	misskeyApi: (endpoint: string, data?: unknown, token?: unknown, signal?: AbortSignal) => misskeyApi(endpoint, data, token, signal),
}));

const DEBOUNCE_MS = 1000;

function callsTo(endpoint: string): number {
	return misskeyApi.mock.calls.filter(call => call[0] === endpoint).length;
}

function inputOf(view: RenderResult, testId: string): HTMLInputElement {
	const input = view.getByTestId(testId).querySelector('input');
	if (input == null) throw new Error(`no input in ${testId}`);
	return input;
}

// 打鍵してから debounce の待ち時間を進め、応答の Promise を解決させる。
async function typeAndSettle(input: HTMLInputElement, value: string): Promise<void> {
	await fireEvent.update(input, value);
	await vi.advanceTimersByTimeAsync(DEBOUNCE_MS + 10);
}

function submitButton(view: RenderResult): HTMLButtonElement {
	return view.getByTestId('signup-submit') as HTMLButtonElement;
}

let savedEmailRequired: boolean | undefined;

beforeEach(() => {
	vi.useFakeTimers();
	misskeyApi.mockClear();
	savedEmailRequired = instance.emailRequiredForSignup;
});

afterEach(() => {
	cleanup();
	vi.useRealTimers();
	instance.emailRequiredForSignup = savedEmailRequired as boolean;
});

function renderForm(): RenderResult {
	return render(XSignupForm, {
		global: { directives, components },
	});
}

// 送信ボタンを有効にするためにパスワードの一致まで済ませる。
async function fillPasswords(view: RenderResult): Promise<void> {
	await fireEvent.update(inputOf(view, 'signup-password'), 'correct-horse-battery');
	await fireEvent.update(inputOf(view, 'signup-password-retype'), 'correct-horse-battery');
}

describe('MkSignupDialog.form username availability check', () => {
	test.each([
		{ name: 'clearing the field', bad: '' },
		{ name: 'invalid characters', bad: 'alice!' },
		{ name: 'a name that is too long', bad: 'a'.repeat(21) },
	])('checks again after $name', async ({ bad }) => {
		instance.emailRequiredForSignup = false;
		const view = renderForm();
		await fillPasswords(view);
		const input = inputOf(view, 'signup-username');

		await typeAndSettle(input, 'alice');
		expect(callsTo('username/available')).toBe(1);
		expect(view.queryByText(i18n.ts.available)).not.toBeNull();

		// 有効な値以外を通すと cancelUsernameCheck() が呼ばれる
		await typeAndSettle(input, bad);
		expect(callsTo('username/available')).toBe(1);
		expect(submitButton(view).disabled).toBe(true);

		await typeAndSettle(input, 'alice_2');
		expect(callsTo('username/available')).toBe(2);
		expect(misskeyApi.mock.calls.at(-1)?.[1]).toEqual({ username: 'alice_2' });
		expect(view.queryByText(i18n.ts.checking)).toBeNull();
		expect(view.queryByText(i18n.ts.available)).not.toBeNull();
		expect(submitButton(view).disabled).toBe(false);
	});

	test('checks again when the first input is too short', async () => {
		instance.emailRequiredForSignup = false;
		// 最小文字数を 3 にして、1 文字の名前を短すぎる名前として通す
		const savedMin = (instance as { minimumUsernameLength?: number }).minimumUsernameLength;
		(instance as { minimumUsernameLength?: number }).minimumUsernameLength = 3;
		try {
			const view = renderForm();
			await fillPasswords(view);
			const input = inputOf(view, 'signup-username');

			await typeAndSettle(input, 'a');
			expect(callsTo('username/available')).toBe(0);
			expect(view.queryByText(i18n.tsx.usernameTooShort({ n: 3 }))).not.toBeNull();

			await typeAndSettle(input, 'alice');
			expect(callsTo('username/available')).toBe(1);
			expect(view.queryByText(i18n.ts.checking)).toBeNull();
			expect(view.queryByText(i18n.ts.available)).not.toBeNull();
			expect(submitButton(view).disabled).toBe(false);
		} finally {
			(instance as { minimumUsernameLength?: number }).minimumUsernameLength = savedMin;
		}
	});

	test('a scheduled lookup for a name that is no longer shown does not mark it available', async () => {
		instance.emailRequiredForSignup = false;
		const view = renderForm();
		const input = inputOf(view, 'signup-username');

		// 予約中に空欄にしたら、1 秒後に古い名前の結果で「利用可能」にしない
		await fireEvent.update(input, 'alice');
		await fireEvent.update(input, '');
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS + 10);
		expect(callsTo('username/available')).toBe(0);
		expect(view.queryByText(i18n.ts.available)).toBeNull();
	});
});

describe('MkSignupDialog.form email availability check', () => {
	test('checks again after the field is cleared and retyped', async () => {
		instance.emailRequiredForSignup = true;
		const view = renderForm();
		const input = inputOf(view, 'signup-email');

		await typeAndSettle(input, 'alice@example.com');
		expect(callsTo('email-address/available')).toBe(1);

		await typeAndSettle(input, '');
		expect(callsTo('email-address/available')).toBe(1);

		await typeAndSettle(input, 'bob@example.com');
		expect(callsTo('email-address/available')).toBe(2);
		expect(misskeyApi.mock.calls.at(-1)?.[1]).toEqual({ emailAddress: 'bob@example.com' });
		expect(view.getByTestId('signup-email').textContent).toContain(i18n.ts.available);
		expect(view.getByTestId('signup-email').textContent).not.toContain(i18n.ts.checking);
	});
});

describe('MkSignupDialog.form unmount', () => {
	test('a scheduled lookup does not fire after the form is closed', async () => {
		instance.emailRequiredForSignup = true;
		const view = renderForm();
		await fireEvent.update(inputOf(view, 'signup-username'), 'alice');
		await fireEvent.update(inputOf(view, 'signup-email'), 'alice@example.com');
		view.unmount();
		await vi.advanceTimersByTimeAsync(DEBOUNCE_MS + 10);
		expect(misskeyApi).not.toHaveBeenCalled();
	});
});
