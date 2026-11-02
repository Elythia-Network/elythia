/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { getScrollPosition, getScrollContainer, getStickyBottom, getStickyTop } from '@@/js/scroll.js';
import { getElementOrNull, getNodeOrNull } from '@/utility/get-dom-node-or-null.js';

type MaybeHTMLElement = EventTarget | Node | Element | HTMLElement;

export const isFocusable = (input: MaybeHTMLElement | null | undefined): input is HTMLElement => {
	if (input == null || !(input instanceof HTMLElement)) return false;

	if (input.tabIndex < 0) return false;
	if ('disabled' in input && input.disabled === true) return false;
	if ('readonly' in input && input.readonly === true) return false;

	if (!input.ownerDocument.contains(input)) return false;

	const style = window.getComputedStyle(input);
	if (style.display === 'none') return false;
	if (style.visibility === 'hidden') return false;
	if (style.opacity === '0') return false;
	if (style.pointerEvents === 'none') return false;

	return true;
};

export const focusPrev = (input: MaybeHTMLElement | null | undefined, self = false, scroll = true) => {
	const element = self ? input : getElementOrNull(input)?.previousElementSibling;
	if (element == null) return;
	if (isFocusable(element)) {
		focusOrScroll(element, scroll);
	} else {
		focusPrev(element, false, scroll);
	}
};

export const focusNext = (input: MaybeHTMLElement | null | undefined, self = false, scroll = true) => {
	const element = self ? input : getElementOrNull(input)?.nextElementSibling;
	if (element == null) return;
	if (isFocusable(element)) {
		focusOrScroll(element, scroll);
	} else {
		focusNext(element, false, scroll);
	}
};

// Elythia: 一覧の項目を、外側の箱 (MkDeferredMount の `data-far-item`) の中にあっても前後へ
// たどる (#3941)。focusNext / focusPrev は隣の兄弟しか見ないので、項目を div で包むと兄弟が
// 無くなり、キーボードでノートの間を移れなくなる (本家でも日付の区切りと広告の枝では同じ)。
// 移り先は外側の箱の中のノートに限る。一覧の外のボタンやセレクトへは移らない。上がるのは
// 外側の箱と区切りの div の分まで
const ITEM_CLIMB_LIMIT = 2;
const ITEM_ATTR = 'data-far-item';

const findItem = (input: MaybeHTMLElement | null | undefined, next: boolean): HTMLElement | 'scrolled' | null => {
	let node = getElementOrNull(input);
	for (let climb = 0; node != null && climb <= ITEM_CLIMB_LIMIT; climb++) {
		for (let sibling = next ? node.nextElementSibling : node.previousElementSibling; sibling != null; sibling = next ? sibling.nextElementSibling : sibling.previousElementSibling) {
			const item = sibling.hasAttribute(ITEM_ATTR) ? sibling : sibling.querySelector(`[${ITEM_ATTR}]`);
			if (item == null) continue;
			// 画面から遠くて中身を外した項目は、飛ばさずに画面へ入れて止める。飛ばすと、間の
			// 外した項目を全部越えて遠くへ移る。入れれば mount し直されるので、次に押すと移れる
			if (item.childElementCount === 0) {
				item.scrollIntoView({ block: next ? 'end' : 'start' });
				return 'scrolled';
			}
			// 項目の根は中で最初の tabindex="0" (中の動画なども tabindex="0" だが、文書の順で根が先)
			const root = item.querySelector('[tabindex="0"]');
			if (isFocusable(root)) return root;
		}
		node = node.parentElement;
	}
	return null;
};

/** Whether the element is inside a list item wrapper that `focusNextItem` / `focusPrevItem` can walk. */
export const isInWrappedItem = (input: MaybeHTMLElement | null | undefined): boolean => getElementOrNull(input)?.closest(`[${ITEM_ATTR}]`) != null;

/** Focuses the next item of a list whose items are wrapped by `MkDeferredMount`. */
export const focusNextItem = (input: MaybeHTMLElement | null | undefined, scroll = true) => {
	const element = findItem(input, true);
	if (element != null && element !== 'scrolled') focusOrScroll(element, scroll);
};

/** Focuses the previous item of a list whose items are wrapped by `MkDeferredMount`. */
export const focusPrevItem = (input: MaybeHTMLElement | null | undefined, scroll = true) => {
	const element = findItem(input, false);
	if (element != null && element !== 'scrolled') focusOrScroll(element, scroll);
};

export const focusParent = (input: MaybeHTMLElement | null | undefined, self = false, scroll = true) => {
	const element = self ? input : getNodeOrNull(input)?.parentElement;
	if (element == null) return;
	if (isFocusable(element)) {
		focusOrScroll(element, scroll);
	} else {
		focusParent(element, false, scroll);
	}
};

const focusOrScroll = (element: HTMLElement, scroll: boolean) => {
	if (scroll) {
		const scrollContainer = getScrollContainer(element) ?? window.document.documentElement;
		const scrollContainerTop = getScrollPosition(scrollContainer);
		const stickyTop = getStickyTop(element, scrollContainer);
		const stickyBottom = getStickyBottom(element, scrollContainer);
		const top = element.getBoundingClientRect().top;
		const bottom = element.getBoundingClientRect().bottom;

		let scrollTo = scrollContainerTop;
		if (top < stickyTop) {
			scrollTo += top - stickyTop;
		} else if (bottom > window.innerHeight - stickyBottom) {
			scrollTo += bottom - window.innerHeight + stickyBottom;
		}
		scrollContainer.scrollTo({ top: scrollTo, behavior: 'instant' });
	}

	if (window.document.activeElement !== element) {
		element.focus({ preventScroll: true });
	}
};
