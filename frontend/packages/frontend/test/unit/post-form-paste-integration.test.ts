/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { cleanup, render } from '@testing-library/vue';
import { nextTick } from 'vue';
import MkPostForm from '@/components/MkPostForm.vue';
import { _resetPasteTransformsForTest, launchServerPlugins } from '@/plugin-api.js';

const testState = vi.hoisted(() => ({
	confirm: vi.fn(),
	addFiles: vi.fn(),
	toast: vi.fn(),
}));

vi.mock('@/i.js', async () => {
	const { reactive } = await import('vue');
	const user = reactive({ id: 'paste-test-user', avatarUrl: '', federationStatus: 'on', policies: new Proxy({}, { get: () => false }) });
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
			addFiles: testState.addFiles,
			removeItem: vi.fn(),
			dispose: vi.fn(),
			abortAll: vi.fn(),
			reset: vi.fn(),
		}),
	};
});

vi.mock('@/plugin.js', () => ({ getPluginHandlers: () => [] }));
vi.mock('@/os.js', () => ({ confirm: testState.confirm, toast: testState.toast }));
vi.mock('@/utility/autocomplete.js', () => ({ Autocomplete: class { detach() {} } }));
vi.mock('@/local-storage.js', () => ({ miLocalStorage: {
	getItem: () => null,
	setItem: vi.fn(),
	removeItem: vi.fn(),
	getItemAsJson: () => undefined,
	setItemAsJson: vi.fn(),
} }));
vi.mock('@/utility/misskey-api.js', () => ({ misskeyApi: vi.fn(async () => ({})) }));

async function flush(): Promise<void> {
	await Promise.resolve();
	await nextTick();
	await Promise.resolve();
}

function makePasteEvent(text: string, items: DataTransferItem[] = []): ClipboardEvent {
	const event = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
	Object.defineProperty(event, 'clipboardData', {
		value: { items, getData: (type: string) => type === 'text' ? text : '' },
	});
	return event;
}

async function mountPostForm(initialText = '') {
	return render(MkPostForm, {
		props: { initialText, instant: true, autofocus: false, mock: false },
		global: {
			stubs: { MkEllipsis: true, MkAcct: true, MkTime: true, I18n: true, MkTip: true },
			directives: { 'click-anime': () => {}, tooltip: () => {} },
		},
	});
}

beforeEach(() => {
	Object.defineProperty(document, 'execCommand', { configurable: true, value: () => false });
});

afterEach(() => {
	cleanup();
	_resetPasteTransformsForTest();
	testState.confirm.mockReset();
	testState.addFiles.mockReset();
	testState.toast.mockReset();
	vi.restoreAllMocks();
	if (typeof document.execCommand !== 'function') {
		Object.defineProperty(document, 'execCommand', { configurable: true, value: () => false });
	}
});

describe('MkPostForm paste transform integration', () => {
	test('transforms changed text and leaves unchanged text to native paste', async () => {
		await launchServerPlugins([{
			name: 'example',
			setup(host) { host.postForm.registerPasteTransform('transform', text => text.replace('World', 'Galaxy')); },
		}]);
		const view = await mountPostForm('Hello World');
		const textarea = view.getByTestId('post-form-text') as HTMLTextAreaElement;
		textarea.focus();
		textarea.setSelectionRange(6, 11);
		const changed = makePasteEvent('World');
		textarea.dispatchEvent(changed);
		await flush();
		expect(changed.defaultPrevented).toBe(true);
		expect(textarea.value).toBe('Hello Galaxy');

		_resetPasteTransformsForTest();
		await launchServerPlugins([{
			name: 'unchanged',
			setup(host) { host.postForm.registerPasteTransform('identity', text => text); },
		}]);
		const unchanged = makePasteEvent('native');
		textarea.dispatchEvent(unchanged);
		await flush();
		expect(unchanged.defaultPrevented).toBe(false);
	});

	test.each(['mid-composition', 'composition-end'] as const)('uploads image paste during %s', async state => {
		const transform = vi.fn((text: string) => `${text}!`);
		await launchServerPlugins([{
			name: 'example',
			setup(host) { host.postForm.registerPasteTransform('transform', transform, { notification: () => 'transformed' }); },
		}]);
		const view = await mountPostForm();
		const textarea = view.getByTestId('post-form-text');
		textarea.dispatchEvent(new CompositionEvent(state === 'mid-composition' ? 'compositionupdate' : 'compositionend', { data: state === 'mid-composition' ? '未確定' : '' }));
		const file = new File(['image'], 'image.png', { type: 'image/png' });
		const item = { kind: 'file', getAsFile: () => file } as unknown as DataTransferItem;
		const event = makePasteEvent('ignored text', [item]);
		textarea.dispatchEvent(event);
		await flush();

		expect(event.defaultPrevented).toBe(true);
		expect(transform).not.toHaveBeenCalled();
		expect(testState.addFiles).toHaveBeenCalledOnce();
		expect(testState.toast).not.toHaveBeenCalled();
	});

	test.each([
		['quote URL', `${window.location.origin}/notes/quoted-note`],
		['long text', 'x'.repeat(1001)],
	])('leaves %s paste to the browser during active composition', async (_name, raw) => {
		const transform = vi.fn((text: string) => `${text}!`);
		await launchServerPlugins([{
			name: 'example',
			setup(host) { host.postForm.registerPasteTransform('transform', transform, { notification: () => 'transformed' }); },
		}]);
		const view = await mountPostForm();
		const textarea = view.getByTestId('post-form-text');
		textarea.dispatchEvent(new CompositionEvent('compositionupdate', { data: '未確定' }));
		const event = makePasteEvent(raw);
		textarea.dispatchEvent(event);
		await flush();

		expect(event.defaultPrevented).toBe(false);
		expect(testState.confirm).not.toHaveBeenCalled();
		expect(transform).not.toHaveBeenCalled();
		expect(testState.toast).not.toHaveBeenCalled();
	});

	test('keeps canceled quote paste raw immediately after composition ends', async () => {
		const transform = vi.fn((text: string) => `${text}!`);
		await launchServerPlugins([{
			name: 'example',
			setup(host) { host.postForm.registerPasteTransform('transform', transform, { notification: () => 'transformed' }); },
		}]);
		testState.confirm.mockResolvedValue({ canceled: true });
		const view = await mountPostForm();
		const textarea = view.getByTestId('post-form-text') as HTMLTextAreaElement;
		textarea.dispatchEvent(new CompositionEvent('compositionend', { data: '' }));
		const raw = `${window.location.origin}/notes/quoted-note`;
		const event = makePasteEvent(raw);
		textarea.dispatchEvent(event);
		await flush();

		expect(event.defaultPrevented).toBe(true);
		expect(testState.confirm).toHaveBeenCalledOnce();
		expect(transform).not.toHaveBeenCalled();
		expect(textarea.value).toBe(raw);
		expect(testState.toast).not.toHaveBeenCalled();
	});

	test('keeps long text on the existing attach-as-file path after composition ends', async () => {
		const transform = vi.fn((text: string) => `${text}!`);
		await launchServerPlugins([{
			name: 'example',
			setup(host) { host.postForm.registerPasteTransform('transform', transform, { notification: () => 'transformed' }); },
		}]);
		testState.confirm.mockResolvedValue({ canceled: false });
		const view = await mountPostForm();
		const textarea = view.getByTestId('post-form-text');
		textarea.dispatchEvent(new CompositionEvent('compositionend', { data: '' }));
		const raw = 'x'.repeat(1001);
		const event = makePasteEvent(raw);
		textarea.dispatchEvent(event);
		await flush();

		expect(event.defaultPrevented).toBe(true);
		expect(testState.confirm).toHaveBeenCalledOnce();
		expect(transform).not.toHaveBeenCalled();
		expect(testState.addFiles).toHaveBeenCalledOnce();
		expect(await (testState.addFiles.mock.calls[0][0][0] as File).text()).toBe(raw);
		expect(testState.toast).not.toHaveBeenCalled();
	});
});
