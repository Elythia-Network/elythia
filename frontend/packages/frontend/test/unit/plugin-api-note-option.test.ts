/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import {
	_resetNoteOptionsForTest,
	isNoteOptionSet,
	launchServerPlugins,
	noteOptionLabel,
	noteOptions,
} from '@/plugin-api.js';
import type { NoteOptionDefinition, PluginHost } from '@/plugin-api.js';

vi.mock('@/i.js', async importOriginal => {
	const actual = await importOriginal<typeof import('@/i.js')>();
	return { ...actual, $i: { id: 'test-user', token: 'session-token', policies: {} } };
});
const testInstance = vi.hoisted(() => ({ noteDeliveryPlugins: undefined as string[] | undefined }));
vi.mock('@/instance.js', () => ({ instance: testInstance }));
vi.mock('@/utility/misskey-api.js', async importOriginal => {
	const actual = await importOriginal<typeof import('@/utility/misskey-api.js')>();
	return { ...actual, misskeyApi: vi.fn(async () => []) };
});

beforeEach(() => {
	testInstance.noteDeliveryPlugins = ['alpha', 'targets', 'zeta'];
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

	// サーバーで無効にしたプラグインの指定は notes/create が断るので、出さない (#4202)。
	test('lists only the options of plugins the server accepts', async () => {
		(await hostOf('targets')).postForm.registerNoteOption(option());
		(await hostOf('disabled')).postForm.registerNoteOption(option());
		expect(noteOptions().map(o => o.plugin)).toEqual(['targets']);

		testInstance.noteDeliveryPlugins = [];
		expect(noteOptions()).toEqual([]);

		// 古いサーバーや端末に残った古い meta には項目が無い。
		testInstance.noteDeliveryPlugins = undefined;
		expect(noteOptions()).toEqual([]);
	});

	test('noteOptionLabel falls back to the plugin name when label throws', async () => {
		const host = await hostOf('targets');
		host.postForm.registerNoteOption(option({ label: () => { throw new Error('broken'); } }));
		expect(noteOptionLabel(noteOptions()[0])).toBe('targets');

		_resetNoteOptionsForTest();
		host.postForm.registerNoteOption(option({ label: () => 42 as unknown as string }));
		expect(noteOptionLabel(noteOptions()[0])).toBe('targets');

		_resetNoteOptionsForTest();
		host.postForm.registerNoteOption(option());
		expect(noteOptionLabel(noteOptions()[0])).toBe('Targets');
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
