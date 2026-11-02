/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// キーボードで一覧の項目を前後に移るとき、項目を包む div があっても移れることを固定する (#3941)。

import { afterEach, describe, expect, test, vi } from 'vitest';
import { focusNextItem, focusPrevItem, isInWrappedItem } from '@/utility/focus.js';

// タイムラインと同じ形: 一覧の容器 > [外側の箱 > ノート] / [区切りの div > 日付, 外側の箱 > ノート] /
// [中身を外した空の箱] / [外側の箱 > ノート]。一覧の容器の兄弟に「もっと見る」、その外にもボタン
function list() {
	document.body.innerHTML = `
		<button id="outside"></button>
		<div id="outer"><button id="more" tabindex="0"></button>
		<div id="list">
			<div data-far-item><div id="a" tabindex="0"><button tabindex="0">inner</button></div></div>
			<div class="sep"><div class="date">date</div><div data-far-item><div id="b" tabindex="0"></div></div></div>
			<div data-far-item id="far"></div>
			<div data-far-item><div id="c" tabindex="0"></div></div>
		</div></div>`;
	const el = (id: string) => document.getElementById(id)!;
	return { el };
}

afterEach(() => {
	document.body.innerHTML = '';
	vi.restoreAllMocks();
});

describe('focusNextItem / focusPrevItem', () => {
	test('moves to the next item through the wrappers and the date separator', () => {
		const { el } = list();
		focusNextItem(el('a'), false);
		expect(document.activeElement).toBe(el('b'));
	});

	test('moves to the previous item, to its root rather than a control inside', () => {
		const { el } = list();
		focusPrevItem(el('b'), false);
		expect(document.activeElement).toBe(el('a'));
	});

	test('stops at an item whose content is unmounted, bringing it into view', () => {
		const { el } = list();
		const scrolled = vi.fn();
		el('far').scrollIntoView = scrolled;
		el('b').focus();
		focusNextItem(el('b'), false);
		// 飛ばして遠くの項目へ移らない
		expect(document.activeElement).toBe(el('b'));
		expect(scrolled).toHaveBeenCalledOnce();

		// mount し直されたら、次に押すとそこへ移る
		el('far').innerHTML = '<div id="d" tabindex="0"></div>';
		focusNextItem(el('b'), false);
		expect(document.activeElement).toBe(el('d'));
	});

	test('does not move to anything outside the items', () => {
		const { el } = list();
		el('a').focus();
		// 一覧の容器の兄弟の「もっと見る」や、その外のボタンへは移らない
		focusPrevItem(el('a'), false);
		expect(document.activeElement).toBe(el('a'));
		el('c').focus();
		focusNextItem(el('c'), false);
		expect(document.activeElement).toBe(el('c'));
	});

	test('tells whether an element is inside a wrapped item', () => {
		const { el } = list();
		expect(isInWrappedItem(el('a'))).toBe(true);
		expect(isInWrappedItem(el('more'))).toBe(false);
	});
});
