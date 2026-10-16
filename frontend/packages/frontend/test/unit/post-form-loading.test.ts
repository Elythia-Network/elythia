/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, test, vi } from 'vitest';
import { unref } from 'vue';

// 投稿フォームのダイアログを遅延読み込みにした os.post() の振る舞い (#3613)。
const h = vi.hoisted(() => {
	let release!: () => void;
	const gate = new Promise<void>(resolve => { release = resolve; });
	return { gate, release, FakeDialog: { name: 'FakePostFormDialog' }, FakeForm: { name: 'FakePostForm' } };
});

vi.mock('@/components/MkPostFormDialog.vue', async () => {
	await h.gate;
	return { default: h.FakeDialog };
});
vi.mock('@/components/MkPostForm.vue', () => ({ default: h.FakeForm }));
vi.mock('@/utility/please-login.js', () => ({ pleaseLogin: vi.fn(async () => true) }));
vi.mock('@/utility/show-moved-dialog.js', () => ({ showMovedDialog: vi.fn() }));

import * as os from '@/os.js';
import MkWaitingDialog from '@/components/MkWaitingDialog.vue';

const flushMicrotasks = async () => {
	for (let i = 0; i < 20; i++) await Promise.resolve();
};
const dialogs = () => os.popups.value.filter(p => p.component === h.FakeDialog);
const waitingDialogs = () => os.popups.value.filter(p => p.component === MkWaitingDialog);

describe('os.post with a lazily loaded dialog', () => {
	test('while loading: shows the waiting dialog, closes it, and pressing twice opens one dialog with the last props', async () => {
		void os.post({ initialText: 'first' });
		void os.post({ initialText: 'second' });
		await flushMicrotasks();
		expect(dialogs()).toHaveLength(0);

		// 100ms を超えて読み込みが続けば待機の表示を出す
		await new Promise(resolve => setTimeout(resolve, 150));
		const waiting = waitingDialogs().at(-1);
		expect(waiting).toBeDefined();
		expect(window.document.body.hasAttribute('inert')).toBe(true);

		h.release();
		await vi.waitFor(() => expect(dialogs()).toHaveLength(1));
		await flushMicrotasks();
		expect(dialogs()).toHaveLength(1);
		// 最後に押したものを開く
		expect(dialogs()[0].props.initialText).toBe('second');
		// 待機の表示を閉じる
		expect(unref(waiting!.props.showing)).toBe(false);
	});

	test('once loaded: opens the dialog without waiting for a task (iOS autofocus)', async () => {
		await os.preloadPostFormDialog();
		const before = dialogs().length;
		void os.post();
		// pleaseLogin の await の後、マイクロタスクの中で開く (setTimeout などのタスクを待たない)
		await flushMicrotasks();
		expect(dialogs()).toHaveLength(before + 1);
	});

	test('the preload also makes the fixed post form of the timeline available synchronously', async () => {
		await os.preloadPostFormDialog();
		expect(os.postFormComponent.loaded).toBe(h.FakeForm);
	});
});

// 起動のとき、ログイン中は空き時間に先読みする (main-boot.ts)。mainBoot はテストで動かせないので
// 呼び出しの形をソースで見る。
describe('main-boot preloads the post form dialog', () => {
	test('preloadPostFormDialog() is scheduled with runWhenIdle inside the signed-in block', () => {
		const source = readFileSync(join(__dirname, '../../src/boot/main-boot.ts'), 'utf8');
		expect(source).toMatch(/if \(\$i\) \{\s*\/\/[^\n]*\n\s*runWhenIdle\(\(\) => \{\s*preloadPostFormDialog\(\)\.catch\(/);
	});

	test('the timeline uses the loaded fixed post form synchronously', () => {
		const source = readFileSync(join(__dirname, '../../src/pages/timeline.vue'), 'utf8');
		expect(source).toMatch(/const MkPostForm = os\.postFormComponent\.loaded \?\? defineAsyncComponent\(\(\) => os\.postFormComponent\.preload\(\)\);/);
	});

	test('the fixed post form is loaded first thing when the timeline shows it', () => {
		const source = readFileSync(join(__dirname, '../../src/boot/main-boot.ts'), 'utf8');
		expect(source).toMatch(/await common\(async \(\) => \{(?:\s*\/\/[^\n]*)*\s*if \(\$i && prefer\.s\.showFixedPostForm\) \{\s*postFormComponent\.preload\(\)\.catch\(/);
	});
});
