/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, describe, expect, test, vi } from 'vitest';
import {
	_resetNoteOptionsForTest,
	isNoteOptionSet,
	launchServerPlugins,
	noteOptions,
} from '@/plugin-api.js';
import type { NoteOptionDefinition, PluginHost } from '@/plugin-api.js';

vi.mock('@/i.js', async importOriginal => {
	const actual = await importOriginal<typeof import('@/i.js')>();
	return { ...actual, $i: { id: 'test-user', token: 'session-token', policies: {} } };
});
vi.mock('@/utility/misskey-api.js', async importOriginal => {
	const actual = await importOriginal<typeof import('@/utility/misskey-api.js')>();
	return { ...actual, misskeyApi: vi.fn(async () => []) };
});

afterEach(() => {
	_resetNoteOptionsForTest();
	vi.restoreAllMocks();
});

const component = {};

function option(extra: Partial<NoteOptionDefinition> = {}): NoteOptionDefinition {
	return { icon: 'ti ti-world-share', label: () => 'Targets', component, ...extra };
}

async function hostOf(name: string): Promise<PluginHost> {
	let host!: PluginHost;
	await launchServerPlugins([{ name, setup(value) { host = value; } }]);
	return host;
}

describe('post form note options', () => {
	test('registers under the plugin name, ordered by name', async () => {
		(await hostOf('zeta')).postForm.registerNoteOption(option());
		(await hostOf('alpha')).postForm.registerNoteOption(option({ icon: 'ti ti-a' }));
		const got = noteOptions();
		expect(got.map(o => o.plugin)).toEqual(['alpha', 'zeta']);
		expect(got[0].icon).toBe('ti ti-a');
	});

	test('allows one option per plugin, because the plugin name is the pluginOptions key', async () => {
		const host = await hostOf('targets');
		host.postForm.registerNoteOption(option());
		expect(() => host.postForm.registerNoteOption(option())).toThrow(/already registered/);
	});

	test('rejects an option without a label function or a component', async () => {
		const host = await hostOf('targets');
		expect(() => host.postForm.registerNoteOption({ ...option(), label: 'x' as unknown as () => string })).toThrow();
		expect(() => host.postForm.registerNoteOption({ ...option(), component: null as unknown as object })).toThrow();
		expect(noteOptions()).toEqual([]);
	});

	test('isNoteOptionSet uses the plugin judgement and sends the value when it throws', async () => {
		const host = await hostOf('targets');
		host.postForm.registerNoteOption(option({ isSet: v => v === 'yes' }));
		const [registered] = noteOptions();
		expect(isNoteOptionSet(registered, 'yes')).toBe(true);
		expect(isNoteOptionSet(registered, 'no')).toBe(false);

		_resetNoteOptionsForTest();
		host.postForm.registerNoteOption(option({ isSet: () => { throw new Error('broken'); } }));
		const [throwing] = noteOptions();
		expect(isNoteOptionSet(throwing, { mode: 'include' })).toBe(true);
		expect(isNoteOptionSet(throwing, null)).toBe(false);

		_resetNoteOptionsForTest();
		host.postForm.registerNoteOption(option());
		const [plain] = noteOptions();
		expect(isNoteOptionSet(plain, null)).toBe(false);
		expect(isNoteOptionSet(plain, undefined)).toBe(false);
		expect(isNoteOptionSet(plain, 0)).toBe(true);
	});
});
