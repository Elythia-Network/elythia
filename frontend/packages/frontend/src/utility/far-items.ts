/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Elythia: 画面から遠く離れた一覧の項目を見分ける (#3941)。タイムラインは読んだノートを
// 全部 mount したまま持ち、下へ読むほど DOM とメモリが増えていた (約 700 件で 8 万ノード、
// 100MB)。遠い項目は高さだけの空の箱に替え、近付いたら mount し直す (MkDeferredMount)。

import type { InjectionKey } from 'vue';

/** Called when an item becomes far from (`far: true`) or near to the visible area. */
export type FarItemCallback = (far: boolean, height: number) => void;

export type FarItems = {
	/**
	 * Starts watching with `root` (the scroll container, or null for the viewport) and
	 * `list` (the element that holds the items, to see which ones the user touched).
	 */
	connect(root: Element | null, list: Element): void;
	/** Stops watching, keeping the registered items (for a list kept alive in the background). */
	pause(): void;
	/** Starts watching again after `pause()`. */
	resume(): void;
	/** Stops everything. */
	disconnect(): void;
	/** Registers an item. Returns a function that unregisters it. */
	observe(el: Element, callback: FarItemCallback): () => void;
	/** Whether the user touched (clicked, opened the context menu, or typed in) the item. */
	isTouched(el: Element): boolean;
};

export const FAR_ITEMS: InjectionKey<FarItems> = Symbol('farItems');

const TOUCH_EVENTS = ['click', 'contextmenu', 'keydown'];
// キーボードでノートの間を移るキー (MkNote の keymap とスクロール)。数えると、通っただけの
// 項目が外れなくなる
const MOVE_KEYS = new Set(['ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'j', 'k', 'Tab', 'PageUp', 'PageDown', 'Home', 'End', ' ', 'Escape', 'Shift', 'Control', 'Alt', 'Meta']);

/**
 * A per-item store that outlives the item's content while it is unmounted for being far.
 * Components inside an item keep state here that must survive the remount.
 */
export const ITEM_STATE: InjectionKey<Map<string, unknown>> = Symbol('itemState');

/**
 * Registers a function to run when the item itself (not just its content) goes away.
 * Components use it for subscriptions that must keep running while the item is unmounted
 * for being far, so that the state kept in `ITEM_STATE` stays current.
 */
export const ITEM_DISPOSE: InjectionKey<(dispose: () => void) => void> = Symbol('itemDispose');

/** The attribute that marks an item element, to find it from an event target. */
export const FAR_ITEM_ATTR = 'data-far-item';

/**
 * Creates a watcher for one list.
 *
 * `margin` is how far beyond the visible area still counts as near, as a CSS margin
 * relative to the root (for example `300%` means three screens above and below).
 */
export function createFarItems(margin = '300%'): FarItems {
	const callbacks = new Map<Element, FarItemCallback>();
	const touched = new WeakSet<Element>();
	let observer: IntersectionObserver | null = null;
	let root: Element | null = null;
	let list: Element | null = null;
	let connected = false;
	let paused = false;

	// 触った項目は外さない。中の部品が持つ状態 (CW を開いた、センシティブな画像を表示した、
	// 投票の結果を見たなど) は部品ごとに散らばっていて、外すと消えるため。リスナーは項目ごと
	// ではなく一覧に 1 つだけ置く。pointerdown では数えない。タッチでスクロールすると、指を
	// 置いた項目に必ず出るので、はじくたびに外れない項目が増えていく (レビューで実測)。
	// click はタップでだけ出る。focusin も数えない。キーボードで移動すると通った項目が全部
	// 外れなくなる。フォーカスを中に持つ項目は、外す側 (MkDeferredMount) で除く
	function onTouch(ev: Event) {
		// CapsLock 中の J / K でも移れる (hotkey は小文字にして照合する) ので、小文字にして見る
		const key = (ev as KeyboardEvent).key;
		if (ev.type === 'keydown' && key != null && MOVE_KEYS.has(key.length === 1 ? key.toLowerCase() : key)) return;
		const item = (ev.target as Element | null)?.closest?.(`[${FAR_ITEM_ATTR}]`);
		if (item != null) touched.add(item);
	}

	function onEntries(entries: IntersectionObserverEntry[]) {
		for (const entry of entries) {
			// KeepAlive は onDeactivated より先に DOM を切り離す。切り離された項目は交差しない
			// と報告されるが、遠いわけではないので外さない
			if (!entry.target.isConnected) continue;
			callbacks.get(entry.target)?.(!entry.isIntersecting, entry.boundingClientRect.height);
		}
	}

	function start() {
		if (!connected || paused || observer != null) return;
		observer = new IntersectionObserver(onEntries, { root, rootMargin: `${margin} 0px` });
		for (const el of callbacks.keys()) observer.observe(el);
	}

	function stop() {
		observer?.disconnect();
		observer = null;
	}

	return {
		connect(newRoot, newList) {
			if (connected) this.disconnect();
			root = newRoot;
			list = newList;
			connected = true;
			for (const type of TOUCH_EVENTS) list.addEventListener(type, onTouch, { capture: true, passive: true });
			start();
		},
		pause() {
			paused = true;
			stop();
		},
		resume() {
			paused = false;
			start();
		},
		disconnect() {
			stop();
			if (list != null) {
				for (const type of TOUCH_EVENTS) list.removeEventListener(type, onTouch, { capture: true });
			}
			list = null;
			connected = false;
		},
		observe(el, callback) {
			callbacks.set(el, callback);
			observer?.observe(el);
			return () => {
				callbacks.delete(el);
				observer?.unobserve(el);
			};
		},
		isTouched: (el) => touched.has(el),
	};
}
