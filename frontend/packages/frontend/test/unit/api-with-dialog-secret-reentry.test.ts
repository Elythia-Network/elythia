/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test, vi } from 'vitest';

const misskeyApi = vi.hoisted(() => vi.fn());

vi.mock('@/utility/misskey-api.js', () => ({ misskeyApi }));
vi.mock('@/i18n.js', () => {
	const ts = new Proxy({
		secretReentryRequired: 'Secret title',
		secretReentryRequiredDescription: 'Secret description',
	} as Record<string, string>, { get: (t, k) => (typeof k === 'string' && k in t ? t[k] : String(k)) });
	return { i18n: { ts, tsx: new Proxy({}, { get: () => () => '' }) } };
});

import * as os from '@/os.js';

async function lastDialogProps() {
	// promiseDialog は reject を受けてから alert を出す。
	for (let i = 0; i < 10; i++) await Promise.resolve();
	return os.popups.value.at(-1)?.props;
}

describe('apiWithDialog', () => {
	test('replaces SECRET_REENTRY_REQUIRED with the localized text', async () => {
		misskeyApi.mockRejectedValueOnce({
			code: 'SECRET_REENTRY_REQUIRED',
			message: 'Enter smtpPass again when changing where it is sent.',
			id: 'x',
		});
		os.apiWithDialog('admin/update-meta', {}).catch(() => {});
		const props = await lastDialogProps();
		expect(props).toMatchObject({ type: 'error', title: 'Secret title', text: 'Secret description' });
	});
});
