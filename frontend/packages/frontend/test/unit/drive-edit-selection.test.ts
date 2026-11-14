/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ドライブの編集モードで、移動・削除したファイルや、編集モードを切った後のファイルが
// 選択に残らないことを固定する (#4158)。残ると、今のフォルダの一覧から消えた後も
// 見えないまま選択に残り、次の移動で一緒に動いてしまう。

import { afterEach, describe, expect, test, vi } from 'vitest';
import { nextTick } from 'vue';
import { cleanup, fireEvent, render, waitFor, type RenderResult } from '@testing-library/vue';
import type * as Elythia from 'elythia-js';
import { directives } from '@/directives/index.js';
import { components } from '@/components/index.js';
import type { MenuItem } from '@/types/menu.js';
import { globalEvents } from '@/events.js';
import { i18n } from '@/i18n.js';

const mocks = vi.hoisted(() => ({
	files: [] as Elythia.entities.DriveFile[],
	apiWithDialog: vi.fn(),
	popupMenu: vi.fn(),
	selectDriveFolder: vi.fn(),
}));

vi.mock('@/utility/misskey-api.js', () => ({
	misskeyApi: vi.fn(async (endpoint: string) => (endpoint === 'drive/files' ? mocks.files : [])),
}));
vi.mock('@/os.js', async (importOriginal) => ({
	...await importOriginal<typeof import('@/os.js')>(),
	apiWithDialog: (...args: unknown[]) => mocks.apiWithDialog(...args),
	popupMenu: async (items: MenuItem[]) => {
		mocks.popupMenu(items);
		return { dispose: () => {} };
	},
}));
vi.mock('@/utility/drive.js', () => ({
	selectDriveFolder: (...args: unknown[]) => mocks.selectDriveFolder(...args),
	chooseFileFromPcAndUpload: vi.fn(),
}));
vi.mock('@/stream.js', () => ({
	useStream: () => ({ useChannel: () => ({ on: () => {}, dispose: () => {} }) }),
}));
// 端末の設定は起動処理が読み込むもので、テストでは読み込まれていない。無限スクロールと
// アニメーションを切った値を返す。
vi.mock('@/preferences.js', async (importOriginal) => {
	const { ref } = await import('vue');
	return {
		...await importOriginal<typeof import('@/preferences.js')>(),
		prefer: {
			s: new Proxy({}, { get: () => false }),
			r: new Proxy({}, { get: () => ref(false) }),
		},
	};
});
vi.mock('@/components/MkDriveFileThumbnail.vue', () => ({ default: { template: '<div></div>' } }));

import MkDrive from '@/components/MkDrive.vue';

const OTHER_FOLDER = { id: 'folder2', name: 'other', parentId: null } as Elythia.entities.DriveFolder;

function driveFile(id: string): Elythia.entities.DriveFile {
	return {
		id,
		createdAt: '2026-11-01T00:00:00.000Z',
		name: `${id}.png`,
		type: 'image/png',
		md5: id,
		size: 1,
		isSensitive: false,
		blurhash: null,
		properties: {},
		url: `https://local.example/${id}`,
		thumbnailUrl: null,
		comment: null,
		folderId: null,
		folder: null,
		userId: null,
		user: null,
	} as Elythia.entities.DriveFile;
}

async function renderDrive(props: Record<string, unknown> = {}, errorHandler?: (err: unknown) => void, files = ['a', 'b', 'c'].map(driveFile)): Promise<RenderResult> {
	mocks.files = files;
	const view = render(MkDrive, { props, global: { directives, components, config: { errorHandler } } });
	await waitFor(() => expect(view.getAllByTestId('drive-file')).toHaveLength(3));
	return view;
}

function fileEl(view: RenderResult, id: string): HTMLElement {
	const el = view.getAllByTestId('drive-file').find(e => e.title.startsWith(`${id}.png`));
	if (el == null) throw new Error(`${id} is not rendered`);
	return el;
}

function selectedIds(view: RenderResult): string[] {
	const events = view.emitted<[Elythia.entities.DriveFile[]]>('changeSelectedFiles');
	if (events.length === 0) return [];
	return events[events.length - 1][0].map(f => f.id);
}

// shift を押しながら押すと編集モードに入り、以降は押すたびに選択を切り替える。
async function selectInEditMode(view: RenderResult, ...ids: string[]): Promise<void> {
	for (const id of ids) {
		await fireEvent.click(fileEl(view, id), { shiftKey: true });
	}
}

function button(view: RenderResult, icon: string): HTMLButtonElement {
	const el = view.container.querySelector(`.${icon}`)?.closest('button');
	if (el == null) throw new Error(`button with ${icon} is not rendered`);
	return el;
}

async function setEditMode(view: RenderResult, enabled: boolean): Promise<void> {
	await fireEvent.click(view.getByTestId('drive-menu'));
	const items = mocks.popupMenu.mock.calls[mocks.popupMenu.mock.calls.length - 1][0] as MenuItem[];
	const sw = items.find(i => 'type' in i && i.type === 'switch');
	if (sw == null || !('ref' in sw) || sw.ref == null) throw new Error('edit switch is not found');
	sw.ref.value = enabled;
	await nextTick();
}

describe('drive edit mode selection', () => {
	afterEach(() => {
		cleanup();
		vi.clearAllMocks();
	});

	test('moving with the button clears the selection, so the next move takes only the new files', async () => {
		const view = await renderDrive();
		mocks.selectDriveFolder.mockResolvedValue({ canceled: false, folders: [OTHER_FOLDER] });
		mocks.apiWithDialog.mockResolvedValue(undefined);

		await selectInEditMode(view, 'a', 'b');
		expect(selectedIds(view)).toEqual(['a', 'b']);
		await fireEvent.click(button(view, 'ti-folder-symlink'));
		await waitFor(() => expect(selectedIds(view)).toEqual([]));
		expect(mocks.apiWithDialog).toHaveBeenLastCalledWith('drive/files/move-bulk', { fileIds: ['a', 'b'], folderId: OTHER_FOLDER.id });
		expect(view.queryAllByTestId('drive-file')).toHaveLength(1);

		await fireEvent.click(fileEl(view, 'c'));
		await fireEvent.click(button(view, 'ti-folder-symlink'));
		await waitFor(() => expect(mocks.apiWithDialog).toHaveBeenCalledTimes(2));
		expect(mocks.apiWithDialog).toHaveBeenLastCalledWith('drive/files/move-bulk', { fileIds: ['c'], folderId: OTHER_FOLDER.id });
	});

	test('moving into the current folder also clears the selection', async () => {
		const view = await renderDrive();
		mocks.selectDriveFolder.mockResolvedValue({ canceled: false, folders: [] });
		mocks.apiWithDialog.mockResolvedValue(undefined);

		await selectInEditMode(view, 'a', 'b');
		await fireEvent.click(button(view, 'ti-folder-symlink'));
		await waitFor(() => expect(selectedIds(view)).toEqual([]));
		expect(mocks.apiWithDialog).toHaveBeenLastCalledWith('drive/files/move-bulk', { fileIds: ['a', 'b'], folderId: null });
		expect(view.queryAllByTestId('drive-file')).toHaveLength(3);
	});

	test('a failed move keeps the selection', async () => {
		// 失敗はダイアログ (apiWithDialog) が出し、クリックの処理からは Vue の errorHandler へ上がる。
		const errorHandler = vi.fn();
		const view = await renderDrive({}, errorHandler);
		mocks.selectDriveFolder.mockResolvedValue({ canceled: false, folders: [OTHER_FOLDER] });
		const failure = new Error('failed');
		mocks.apiWithDialog.mockRejectedValue(failure);

		await selectInEditMode(view, 'a', 'b');
		await fireEvent.click(button(view, 'ti-folder-symlink'));
		await waitFor(() => expect(mocks.apiWithDialog).toHaveBeenCalled());
		await waitFor(() => expect(errorHandler).toHaveBeenCalledWith(failure, expect.anything(), expect.anything()));
		expect(selectedIds(view)).toEqual(['a', 'b']);
		expect(view.queryAllByTestId('drive-file')).toHaveLength(3);
	});

	test('files moved out of the folder (by drag and drop) leave the selection; files updated in place stay', async () => {
		const view = await renderDrive();
		await selectInEditMode(view, 'a', 'b');

		globalEvents.emit('driveFilesUpdated', [
			{ ...driveFile('a'), folderId: OTHER_FOLDER.id, folder: OTHER_FOLDER },
			{ ...driveFile('b'), name: 'renamed.png' },
		]);
		await nextTick();
		expect(selectedIds(view)).toEqual(['b']);
	});

	test('a file selected in another folder stays selected when it is updated without moving', async () => {
		// 選択はフォルダをまたいで残る。別のフォルダで選んだファイルの名前を変えても外さない。
		// 今のフォルダの一覧に、別のフォルダのファイルとして c を出して選ぶ。
		const view = await renderDrive({}, undefined, [driveFile('a'), driveFile('b'), { ...driveFile('c'), folderId: OTHER_FOLDER.id, folder: OTHER_FOLDER }]);
		await selectInEditMode(view, 'c');

		globalEvents.emit('driveFilesUpdated', [{ ...driveFile('c'), name: 'renamed.png', folderId: OTHER_FOLDER.id, folder: OTHER_FOLDER }]);
		await nextTick();
		expect(selectedIds(view)).toEqual(['c']);
	});

	test('deleted files leave the selection', async () => {
		const view = await renderDrive();
		await selectInEditMode(view, 'a', 'b');

		globalEvents.emit('driveFilesDeleted', [driveFile('a')]);
		await nextTick();
		expect(selectedIds(view)).toEqual(['b']);
	});

	test('the clear button unselects every file and shows the count', async () => {
		const view = await renderDrive();
		await setEditMode(view, true);
		const clear = view.getByTestId('drive-unselect-all') as HTMLButtonElement;
		expect(clear.disabled).toBe(true);
		expect(view.getByTestId('drive-selected-count').textContent).toBe(i18n.tsx.nFilesSelected({ n: 0 }));

		await selectInEditMode(view, 'a', 'c');
		expect(view.getByTestId('drive-selected-count').textContent).toBe(i18n.tsx.nFilesSelected({ n: 2 }));
		expect(clear.disabled).toBe(false);
		await fireEvent.click(clear);
		expect(selectedIds(view)).toEqual([]);
	});

	test('turning edit mode off clears the selection', async () => {
		const view = await renderDrive();
		await selectInEditMode(view, 'a', 'b');

		await setEditMode(view, false);
		expect(selectedIds(view)).toEqual([]);
	});

	test('the file picker keeps its selection when edit mode is turned off or files move to another folder', async () => {
		const view = await renderDrive({ select: 'file', multiple: true });
		await fireEvent.click(fileEl(view, 'a'));
		await fireEvent.click(fileEl(view, 'b'));
		expect(selectedIds(view)).toEqual(['a', 'b']);

		globalEvents.emit('driveFilesUpdated', [{ ...driveFile('a'), folderId: OTHER_FOLDER.id, folder: OTHER_FOLDER }]);
		await nextTick();
		expect(selectedIds(view)).toEqual(['a', 'b']);

		await setEditMode(view, true);
		await setEditMode(view, false);
		expect(selectedIds(view)).toEqual(['a', 'b']);

		// 消したファイルは、ダイアログでも結果に残さない。
		globalEvents.emit('driveFilesDeleted', [driveFile('a')]);
		await nextTick();
		expect(selectedIds(view)).toEqual(['b']);
	});
});
