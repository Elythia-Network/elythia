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
import { prefer } from '@/preferences.js';
import { i18n } from '@/i18n.js';

// rememberNoteVisibility を張り替える。プロセス全体で共有されるので、afterEach で戻す。
const originalRememberNoteVisibility = prefer.s.rememberNoteVisibility;
function setRememberNoteVisibility(value: boolean) {
	(prefer.s as { rememberNoteVisibility: boolean }).rememberNoteVisibility = value;
}

type MenuItem = { text?: string; icon?: string; action?: () => unknown };

const testState = vi.hoisted(() => ({
	alert: vi.fn(),
	confirm: vi.fn(async () => ({ canceled: false })),
	apiWithDialog: vi.fn(async () => ({})),
	misskeyApi: vi.fn(async () => ({ createdNote: { id: 'created' } })),
	inputDatetime: vi.fn(async () => ({ canceled: false, result: new Date(Date.now() + 60 * 60 * 1000) })),
	menuItems: [] as MenuItem[],
	storage: new Map<string, string>(),
	dialogValue: { mode: 'include', hosts: ['a.example'] } as unknown,
	// store に覚えた指定 (#4205)。
	remembered: {} as Record<string, unknown>,
	// os.popup に渡されたイベント (下書きの一覧の restore を呼ぶため)。
	popupEvents: null as Record<string, (...args: unknown[]) => unknown> | null,
	// 端末に保存したアカウントと、アカウントのメニューで選んだときの処理。
	accounts: [] as { id: string; token: string }[],
	chooseAccount: null as ((account: { id: string }) => void) | null,
	dialogTitles: [] as unknown[],
	noteDeliveryPlugins: ['targets', 'other'] as string[],
}));

vi.mock('@/accounts.js', () => ({
	getAccounts: async () => testState.accounts,
	getAccountMenu: async (opts: { onChoose: (account: { id: string }) => void }) => {
		testState.chooseAccount = opts.onChoose;
		return [];
	},
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
			s: {
				showPreview: false,
				localOnly: false,
				visibility: 'public',
				reactionAcceptance: 'nonSensitiveOnly',
				get pluginNoteOptions() { return testState.remembered; },
			},
			model(key: string) {
				if (!models.has(key)) models.set(key, ref(key === 'postFormWithHashtags' ? false : ''));
				return models.get(key);
			},
			set: (key: string, value: unknown) => {
				if (key === 'pluginNoteOptions') testState.remembered = value as Record<string, unknown>;
			},
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
	return {
		...actual,
		instance: {
			...actual.instance,
			maxNoteTextLength: 3000,
			get noteDeliveryPlugins() { return testState.noteDeliveryPlugins; },
		},
	};
});
vi.mock('@/plugin.js', () => ({ getPluginHandlers: () => [] }));
vi.mock('@/os.js', () => ({
	alert: testState.alert,
	confirm: testState.confirm,
	toast: vi.fn(),
	apiWithDialog: testState.apiWithDialog,
	inputDatetime: testState.inputDatetime,
	popupMenu: (items: MenuItem[]) => { testState.menuItems = items; },
	popup: (_component: unknown, _props: unknown, events: Record<string, (...args: unknown[]) => unknown>) => {
		testState.popupEvents = events;
		return { dispose: vi.fn() };
	},
	// 指定のダイアログは、開いたらすぐに testState.dialogValue で閉じる。
	popupAsyncWithDialog: async (_component: unknown, props: { title?: unknown }, events: { done: (result: { canceled: boolean; value: unknown }) => void }) => {
		testState.dialogTitles.push(props.title);
		events.done({ canceled: false, value: testState.dialogValue });
		return { dispose: vi.fn() };
	},
}));
vi.mock('@/utility/autocomplete.js', () => ({ Autocomplete: class { detach() {} } }));
vi.mock('@/local-storage.js', () => ({ miLocalStorage: {
	getItem: (key: string) => testState.storage.get(key) ?? null,
	setItem: (key: string, value: string) => { testState.storage.set(key, value); },
	removeItem: (key: string) => { testState.storage.delete(key); },
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

async function registerOption(name: string, label: () => string = () => name) {
	await launchServerPlugins([{
		name,
		setup(host) {
			host.postForm.registerNoteOption({ icon: 'ti ti-world-share', label, component: {} });
		},
	}]);
}

// instant: false のときだけ、端末に書きかけの投稿を残して復元する。
async function mountWithOption(options: { plugin?: string; instant?: boolean; label?: () => string; props?: Record<string, unknown> } = {}) {
	await registerOption(options.plugin ?? 'targets', options.label);
	const view = render(MkPostForm, {
		props: { initialText: 'hello', instant: options.instant ?? true, autofocus: false, mock: false, ...options.props },
		global: {
			stubs: { MkEllipsis: true, MkAcct: true, MkTime: true, I18n: true, MkTip: true },
			directives: { 'click-anime': () => {}, tooltip: () => {} },
		},
	});
	await flush();
	return view;
}

async function setOption(view: Awaited<ReturnType<typeof mountWithOption>>, plugin = 'targets') {
	await fireEvent.click(view.getByTestId(`post-form-plugin-option-${plugin}`));
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
	testState.dialogTitles = [];
	testState.noteDeliveryPlugins = ['targets', 'other'];
	testState.menuItems = [];
	testState.popupEvents = null;
	testState.storage.clear();
	testState.remembered = {};
	testState.accounts = [];
	testState.chooseAccount = null;
	setRememberNoteVisibility(false);
});

afterEach(() => {
	setRememberNoteVisibility(originalRememberNoteVisibility);
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

	test('the local draft keeps the option and restores it', async () => {
		const first = await mountWithOption({ instant: false });
		await setOption(first);
		const stored = JSON.parse(testState.storage.get('drafts') ?? '{}') as Record<string, { data: { pluginOptions?: unknown } }>;
		expect(Object.values(stored).map(x => x.data.pluginOptions)).toEqual([{ targets: { mode: 'include', hosts: ['a.example'] } }]);
		cleanup();
		_resetNoteOptionsForTest();

		const view = await mountWithOption({ instant: false });
		expect(testState.alert).not.toHaveBeenCalled();
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		const payload = (createCalls()[0] as unknown[])[1] as { pluginOptions?: unknown };
		expect(payload.pluginOptions).toEqual({ targets: { mode: 'include', hosts: ['a.example'] } });
	});

	test('restoring drops the option of a plugin no longer registered and says so', async () => {
		const first = await mountWithOption({ instant: false });
		await setOption(first);
		cleanup();
		_resetNoteOptionsForTest();

		const view = await mountWithOption({ instant: false, plugin: 'other' });
		expect(testState.alert).toHaveBeenCalledTimes(1);
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		const payload = (createCalls()[0] as unknown[])[1] as { pluginOptions?: unknown };
		expect(payload.pluginOptions).toBeUndefined();
	});

	// サーバーで無効にしたプラグインは、ボタンを出さない (#4202)。出すと、指定した
	// 投稿が毎回 INVALID_PARAM になる。
	test('a plugin disabled on the server shows no button', async () => {
		testState.noteDeliveryPlugins = ['other'];
		const view = await mountWithOption();
		expect(view.queryByTestId('post-form-plugin-option-targets')).toBeNull();
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		const payload = (createCalls()[0] as unknown[])[1] as { pluginOptions?: unknown };
		expect(payload.pluginOptions).toBeUndefined();
	});

	test('restoring drops the option of a plugin disabled on the server and says so', async () => {
		const first = await mountWithOption({ instant: false });
		await setOption(first);
		cleanup();
		_resetNoteOptionsForTest();

		testState.noteDeliveryPlugins = [];
		const view = await mountWithOption({ instant: false });
		expect(testState.alert).toHaveBeenCalledTimes(1);
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		const payload = (createCalls()[0] as unknown[])[1] as { pluginOptions?: unknown };
		expect(payload.pluginOptions).toBeUndefined();
	});

	// label() が例外を投げても、フォームは描画でき、ダイアログの題にはプラグインの名前を出す (#4202)。
	test('a label that throws does not break the form', async () => {
		const view = await mountWithOption({ label: () => { throw new Error('broken'); } });
		await setOption(view);
		expect(testState.dialogTitles).toEqual(['targets']);
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		const payload = (createCalls()[0] as unknown[])[1] as { pluginOptions?: Record<string, unknown> };
		expect(payload.pluginOptions).toEqual({ targets: { mode: 'include', hosts: ['a.example'] } });
	});
});

// rememberNoteVisibility がオンなら、送った指定を覚えて次に開いたフォームの初期値にする (#4205)。
describe('MkPostForm remembers plugin note options like the visibility', () => {
	const sent = { targets: { mode: 'include', hosts: ['a.example'] } };

	async function submit(view: Awaited<ReturnType<typeof mountWithOption>>) {
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		const calls = createCalls();
		return ((calls[calls.length - 1] as unknown[])[1] as { pluginOptions?: unknown }).pluginOptions;
	}

	async function remount(options: Parameters<typeof mountWithOption>[0] = {}) {
		cleanup();
		_resetNoteOptionsForTest();
		return mountWithOption(options);
	}

	test('the next form starts from the options sent when the setting is on', async () => {
		setRememberNoteVisibility(true);
		const first = await mountWithOption();
		await setOption(first);
		expect(await submit(first)).toEqual(sent);
		expect(testState.remembered).toEqual(sent);

		const next = await remount();
		expect(await submit(next)).toEqual(sent);
	});

	test('the next form starts empty when the setting is off', async () => {
		const first = await mountWithOption();
		await setOption(first);
		expect(await submit(first)).toEqual(sent);
		expect(testState.remembered).toEqual({});

		const next = await remount();
		expect(await submit(next)).toBeUndefined();
	});

	// 黙って外すと、絞ったつもりのノートが絞られないまま投稿されうるので、書きかけの
	// 投稿と同じく知らせる。覚えた指定からも外し、次のフォームでは知らせ直さない (#4202)。
	test('options of plugins not registered now are dropped and it says so once', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = { ...sent, gone: { mode: 'exclude', hosts: ['b.example'] } };
		const view = await mountWithOption();
		expect(testState.alert).toHaveBeenCalledTimes(1);
		expect(testState.alert).toHaveBeenLastCalledWith(expect.objectContaining({ text: i18n.ts.pluginNoteOptionsDroppedFromRemembered }));
		expect(testState.remembered).toEqual(sent);
		expect(await submit(view)).toEqual(sent);

		testState.alert.mockClear();
		await remount();
		expect(testState.alert).not.toHaveBeenCalled();
	});

	test('options of a plugin disabled on the server are dropped and it says so', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = sent;
		testState.noteDeliveryPlugins = ['other'];
		const view = await mountWithOption();
		expect(testState.alert).toHaveBeenCalledTimes(1);
		expect(testState.alert).toHaveBeenLastCalledWith(expect.objectContaining({ text: i18n.ts.pluginNoteOptionsDroppedFromRemembered }));
		expect(testState.remembered).toEqual({});
		expect(await submit(view)).toBeUndefined();
	});

	// フォームに出ている指定と関係のない知らせは出さない。覚えた指定からは外す。
	test('a form not starting from them does not tell about the dropped ones', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = { ...sent, gone: { mode: 'exclude', hosts: ['b.example'] } };
		await mountWithOption({ props: {
			initialNote: { id: 'old', text: 'hello', cw: null, visibility: 'public', localOnly: false, files: [], poll: null, reactionAcceptance: null },
		} });
		expect(testState.alert).not.toHaveBeenCalled();
		expect(testState.remembered).toEqual(sent);
	});

	test('a value remembered earlier is not used once the setting is off', async () => {
		testState.remembered = sent;
		const view = await mountWithOption();
		expect(await submit(view)).toBeUndefined();
		expect(testState.remembered).toEqual(sent);
	});

	// 覚える先はログイン中のアカウントの記憶なので、別のアカウントとして投稿した
	// 指定は覚えない (#4205)。
	test('posting as another account remembers nothing', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = { targets: { mode: 'exclude', hosts: ['c.example'] } };
		testState.accounts = [{ id: 'other-account', token: 'other-token' }];
		const view = await mountWithOption();
		await fireEvent.click(view.getByTestId('post-form-account'));
		await flush();
		expect(testState.chooseAccount).not.toBeNull();
		testState.chooseAccount!({ id: 'other-account' });
		await flush();
		await setOption(view);
		expect(await submit(view)).toEqual(sent);
		expect(testState.misskeyApi).toHaveBeenLastCalledWith('notes/create', expect.anything(), 'other-token');
		expect(testState.remembered).toEqual({ targets: { mode: 'exclude', hosts: ['c.example'] } });
	});

	test('posting without an option remembers none', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = sent;
		const view = await mountWithOption();
		testState.dialogValue = null;
		await setOption(view);
		expect(await submit(view)).toBeUndefined();
		expect(testState.remembered).toEqual({});
	});

	test('a failed post remembers nothing', async () => {
		setRememberNoteVisibility(true);
		testState.misskeyApi.mockImplementationOnce(async () => { throw new Error('rejected'); });
		const view = await mountWithOption();
		await setOption(view);
		await submit(view);
		expect(testState.remembered).toEqual({});
	});

	test('reset forgets the remembered options', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = sent;
		const view = await mountWithOption();
		await runMenuItem(view, 'ti ti-trash');
		expect(testState.remembered).toEqual({});
	});

	test('a form given its visibility by the caller does not start from them', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = sent;
		const view = await mountWithOption({ props: { initialVisibility: 'home' } });
		expect(await submit(view)).toBeUndefined();
	});

	test('delete and edit does not start from them', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = sent;
		const view = await mountWithOption({ props: {
			initialNote: { id: 'old', text: 'hello', cw: null, visibility: 'public', localOnly: false, files: [], poll: null, reactionAcceptance: null },
		} });
		expect(await submit(view)).toBeUndefined();
	});

	test('a restored server draft replaces them', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = sent;
		const view = await mountWithOption();
		await fireEvent.click(view.getByTestId('post-form-account'));
		await flush();
		const listDrafts = testState.menuItems.find(x => x.icon === 'ti ti-cloud-download');
		expect(listDrafts).toBeDefined();
		listDrafts!.action!();
		await testState.popupEvents!.restore({
			id: 'draft1', text: 'from the server', cw: null, visibility: 'public', localOnly: false, files: [], hashtag: null, poll: null,
			visibleUserIds: [], renoteId: null, renote: null, reply: null, reactionAcceptance: null, scheduledAt: null, channel: null,
		});
		await flush();
		expect(await submit(view)).toBeUndefined();
	});

	test('an ordinary form with no local draft keeps them', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = sent;
		const view = await mountWithOption({ instant: false });
		expect(testState.alert).not.toHaveBeenCalled();
		expect(await submit(view)).toEqual(sent);
	});

	// 覚えた指定から始まったフォームも、自分で付けた指定と同じく下書きと予約投稿で止まる。
	test('a form starting from them stops saving a server draft and scheduling', async () => {
		setRememberNoteVisibility(true);
		testState.remembered = sent;
		const view = await mountWithOption();
		await runMenuItem(view, 'ti ti-cloud-upload');
		// 止めた理由が指定によるものかを、知らせの文言で確かめる。
		expect(testState.alert).toHaveBeenCalledTimes(1);
		expect(testState.alert).toHaveBeenLastCalledWith(expect.objectContaining({ text: i18n.ts.pluginNoteOptionsCannotBeDrafted }));
		expect(testState.apiWithDialog).not.toHaveBeenCalled();

		await runMenuItem(view, 'ti ti-calendar-time');
		await fireEvent.click(view.getByTestId('post-form-submit'));
		await flush();
		expect(testState.alert).toHaveBeenCalledTimes(2);
		expect(testState.alert).toHaveBeenLastCalledWith(expect.objectContaining({ text: i18n.ts.pluginNoteOptionsCannotBeDrafted }));
		expect(testState.apiWithDialog).not.toHaveBeenCalled();
		expect(createCalls()).toHaveLength(0);
	});

	test('a restored local draft replaces them', async () => {
		setRememberNoteVisibility(true);
		const first = await mountWithOption({ instant: false });
		await fireEvent.update(first.getByTestId('post-form-text'), 'draft without options');
		await flush();
		testState.remembered = { ...sent, gone: { mode: 'exclude', hosts: ['b.example'] } };

		const next = await remount({ instant: false });
		expect(testState.alert).not.toHaveBeenCalled();
		expect(await submit(next)).toBeUndefined();
	});
});
