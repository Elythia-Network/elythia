/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームのプラグインの指定 (pluginOptions) が、notes/create には載り、
// 下書きと予約投稿では送られずに止まることを確かめる (#4199)。下書きは
// pluginOptions を受け取れないので、黙って保存すると、その下書きから投稿した
// ときに絞ったつもりのノートがどこへでも届く。

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/vue';
import { nextTick } from 'vue';
import MkPostForm from '@/components/MkPostForm.vue';
import { _resetNoteOptionsForTest, launchServerPlugins } from '@/plugin-api.js';

type MenuItem = { text?: string; icon?: string; action?: () => unknown };

const testState = vi.hoisted(() => ({
	alert: vi.fn(),
	confirm: vi.fn(async () => ({ canceled: false })),
	apiWithDialog: vi.fn(async () => ({})),
	misskeyApi: vi.fn(async () => ({ createdNote: { id: 'created' } })),
	inputDatetime: vi.fn(async () => ({ canceled: false, result: new Date(Date.now() + 60 * 60 * 1000) })),
	menuItems: [] as MenuItem[],
	dialogValue: { mode: 'include', hosts: ['a.example'] } as unknown,
}));

vi.mock('@/i.js', async () => {
	const { reactive } = await import('vue');
	const policies = new Proxy({}, { get: (_target, key) => key === 'scheduledNoteLimit' ? 1 : false });
	const user = reactive({ id: 'option-test-user', avatarUrl: '', federationStatus: 'on', policies });
	return { $i: user, ensureSignin: () => user, notesCount: 0, incNotesCount: vi.fn(), iAmModerator: false, iAmAdmin: false };
});

vi.mock('@/store.js', async () => {
	const { ref } = await import('vue');
	const models = new Map<string, unknown>();
	return {
		store: {
			r: { tips: { value: { postForm: true } } },
			s: { showPreview: false, localOnly: false, visibility: 'public', reactionAcceptance: 'nonSensitiveOnly' },
			model(key: string) {
				if (!models.has(key)) models.set(key, ref(key === 'postFormWithHashtags' ? false : ''));
				return models.get(key);
			},
			set: vi.fn(),
		},
	};
});

vi.mock('@/composables/use-uploader.js', async () => {
	const { ref } = await import('vue');
	return {
		useUploader: () => ({
			items: ref([]),
			uploading: ref(false),
			readyForUpload: ref(true),
			allItemsUploaded: ref(true),
			events: { on: vi.fn() },
			addFiles: vi.fn(),
			removeItem: vi.fn(),
			dispose: vi.fn(),
			abortAll: vi.fn(),
			reset: vi.fn(),
		}),
	};
});

vi.mock('@/instance.js', async importOriginal => {
	const actual = await importOriginal<typeof import('@/instance.js')>();
	return { ...actual, instance: { ...actual.instance, maxNoteTextLength: 3000 } };
});
vi.mock('@/plugin.js', () => ({ getPluginHandlers: () => [] }));
vi.mock('@/os.js', () => ({
	alert: testState.alert,
	confirm: testState.confirm,
	toast: vi.fn(),
	apiWithDialog: testState.apiWithDialog,
	inputDatetime: testState.inputDatetime,
	popupMenu: (items: MenuItem[]) => { testState.menuItems = items; },
	// 指定のダイアログは、開いたらすぐに testState.dialogValue で閉じる。
	popupAsyncWithDialog: async (_component: unknown, _props: unknown, events: { done: (result: { canceled: boolean; value: unknown }) => void }) => {
		events.done({ canceled: false, value: testState.dialogValue });
		return { dispose: vi.fn() };
	},
}));
vi.mock('@/utility/autocomplete.js', () => ({ Autocomplete: class { detach() {} } }));
vi.mock('@/local-storage.js', () => ({ miLocalStorage: {
	getItem: () => null,
	setItem: vi.fn(),
	removeItem: vi.fn(),
	getItemAsJson: () => undefined,
	setItemAsJson: vi.fn(),
} }));
vi.mock('@/utility/misskey-api.js', () => ({ misskeyApi: testState.misskeyApi }));

async function flush(): Promise<void> {
	for (let i = 0; i < 3; i++) {
		await Promise.resolve();
		await nextTick();
	}
}

async function mountWithOption() {
	await launchServerPlugins([{
		name: 'targets',
		setup(host) {
			host.postForm.registerNoteOption({ icon: 'ti ti-world-share', label: () => 'Targets', component: {} });
		},
	}]);
	const view = render(MkPostForm, {
		props: { initialText: 'hello', instant: true, autofocus: false, mock: false },
		global: {
			stubs: { MkEllipsis: true, MkAcct: true, MkTime: true, I18n: true, MkTip: true },
			directives: { 'click-anime': () => {}, tooltip: () => {} },
		},
	});
	await flush();
	return view;
}

async function setOption(view: Awaited<ReturnType<typeof mountWithOption>>) {
	await fireEvent.click(view.getByTestId('post-form-plugin-option-targets'));
	await flush();
}

async function runMenuItem(view: Awaited<ReturnType<typeof mountWithOption>>, icon: string) {
	await fireEvent.click(view.getByTestId('post-form-other-settings'));
	await flush();
	const item = testState.menuItems.find(x => x.icon === icon);
	expect(item, `menu item ${icon}`).toBeDefined();
	await item!.action!();
	await flush();
}

function createCalls() {
	return testState.misskeyApi.mock.calls.filter(call => (call as unknown[])[0] === 'notes/create');
}

beforeEach(() => {
	Object.defineProperty(document, 'execCommand', { configurable: true, value: () => false });
	testState.dialogValue = { mode: 'include', hosts: ['a.example'] };
	testState.menuItems = [];
});

afterEach(() => {
	cleanup();
	_resetNoteOptionsForTest();
	testState.alert.mockClear();
	testState.confirm.mockClear();
	testState.apiWithDialog.mockClear();
	testState.misskeyApi.mockClear();
	testState.inputDatetime.mockClear();
});

describe('MkPostForm plugin note options', () => {
	test('notes/create carries the option under the plugin name', async () => {
		const view = await mountWithOption();
		await setOption(view);
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		expect(createCalls()).toHaveLength(1);
		const payload = (createCalls()[0] as unknown[])[1] as { pluginOptions?: Record<string, unknown> };
		expect(payload.pluginOptions).toEqual({ targets: { mode: 'include', hosts: ['a.example'] } });
	});

	test('notes/create carries no pluginOptions without an option', async () => {
		const view = await mountWithOption();
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		const payload = (createCalls()[0] as unknown[])[1] as { pluginOptions?: unknown };
		expect(payload.pluginOptions).toBeUndefined();
	});

	test('saving a server draft from the menu stops with the option set', async () => {
		const view = await mountWithOption();
		await setOption(view);
		await runMenuItem(view, 'ti ti-cloud-upload');
		expect(testState.alert).toHaveBeenCalledTimes(1);
		expect(testState.apiWithDialog).not.toHaveBeenCalled();
	});

	test('saving a server draft from the menu goes through without an option', async () => {
		const view = await mountWithOption();
		await runMenuItem(view, 'ti ti-cloud-upload');
		expect(testState.alert).not.toHaveBeenCalled();
		expect(testState.apiWithDialog).toHaveBeenCalledTimes(1);
		expect((testState.apiWithDialog.mock.calls[0] as unknown[])[0]).toBe('notes/drafts/create');
	});

	test('a scheduled post stops with the option set and keeps the form', async () => {
		const view = await mountWithOption();
		await setOption(view);
		await runMenuItem(view, 'ti ti-calendar-time');
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		expect(testState.alert).toHaveBeenCalledTimes(1);
		expect(testState.apiWithDialog).not.toHaveBeenCalled();
		expect(createCalls()).toHaveLength(0);
		expect((view.getByTestId('post-form-text') as HTMLTextAreaElement).value).toBe('hello');
	});

	test('reset clears the option', async () => {
		const view = await mountWithOption();
		await setOption(view);
		await runMenuItem(view, 'ti ti-trash');
		await fireEvent.update(view.getByTestId('post-form-text'), 'again');
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		const payload = (createCalls()[0] as unknown[])[1] as { pluginOptions?: unknown };
		expect(payload.pluginOptions).toBeUndefined();
	});
});
