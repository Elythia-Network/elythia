/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 画面から遠い一覧の項目を見分ける仕組みを固定する (#3941)。

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { createFarItems, FAR_ITEM_ATTR } from '@/utility/far-items.js';

// IntersectionObserver を、テストから交差を報告できる偽物にする
class FakeObserver {
	static instances: FakeObserver[] = [];
	observed = new Set<Element>();
	disconnected = false;
	constructor(public callback: IntersectionObserverCallback, public options: IntersectionObserverInit = {}) {
		FakeObserver.instances.push(this);
	}
	observe(el: Element) {
		this.observed.add(el);
	}
	unobserve(el: Element) {
		this.observed.delete(el);
	}
	disconnect() {
		this.disconnected = true;
		this.observed.clear();
	}
	report(el: Element, isIntersecting: boolean, height = 100) {
		this.callback([{ target: el, isIntersecting, boundingClientRect: { height } as DOMRectReadOnly } as IntersectionObserverEntry], this as unknown as IntersectionObserver);
	}
}

const live = () => FakeObserver.instances.filter(o => !o.disconnected);

function setup() {
	const list = document.createElement('div');
	const root = document.createElement('div');
	root.append(list);
	document.body.append(root);
	const item = (text = 'x') => {
		const el = document.createElement('div');
		el.setAttribute(FAR_ITEM_ATTR, '');
		el.append(document.createElement('button'), text);
		list.append(el);
		return el;
	};
	return { list, root, item };
}

beforeEach(() => {
	FakeObserver.instances = [];
	vi.stubGlobal('IntersectionObserver', FakeObserver);
});

afterEach(() => {
	vi.unstubAllGlobals();
	document.body.innerHTML = '';
});

describe('createFarItems', () => {
	test('watches the items registered before connecting, relative to the scroll container', () => {
		const { list, root, item } = setup();
		const far = createFarItems('300%');
		const a = item();
		far.observe(a, () => {});
		expect(live()).toHaveLength(0);

		far.connect(root, list);
		expect(live()).toHaveLength(1);
		expect(live()[0].options).toEqual({ root, rootMargin: '300% 0px' });
		expect(live()[0].observed.has(a)).toBe(true);
	});

	test('reports far when the item leaves the margin, with its height', () => {
		const { list, root, item } = setup();
		const far = createFarItems();
		const a = item();
		const cb = vi.fn();
		far.observe(a, cb);
		far.connect(root, list);

		live()[0].report(a, false, 321);
		expect(cb).toHaveBeenLastCalledWith(true, 321);
		live()[0].report(a, true, 321);
		expect(cb).toHaveBeenLastCalledWith(false, 321);
	});

	test('ignores items detached from the document (a list kept alive in the background)', () => {
		const { list, root, item } = setup();
		const far = createFarItems();
		const a = item();
		const cb = vi.fn();
		far.observe(a, cb);
		far.connect(root, list);
		a.remove();
		live()[0].report(a, false);
		expect(cb).not.toHaveBeenCalled();
	});

	test.each(['pointerdown', 'focusin'])('does not count %s as a touch', (type) => {
		// タッチでスクロールすると、指を置いた項目に pointerdown が出る。キーボードで移動すると
		// focusin が出る。どちらも数えると、通っただけの項目が外れなくなる
		const { list, root, item } = setup();
		const far = createFarItems();
		const a = item();
		far.connect(root, list);
		a.querySelector('button')!.dispatchEvent(new Event(type, { bubbles: true }));
		expect(far.isTouched(a)).toBe(false);
	});

	test.each(['ArrowDown', 'j', 'k', 'J', 'K', 'Tab', ' '])('does not count the key %j that moves between items', (key) => {
		const { list, root, item } = setup();
		const far = createFarItems();
		const a = item();
		far.connect(root, list);
		a.querySelector('button')!.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true }));
		expect(far.isTouched(a)).toBe(false);
	});

	test.each(['click', 'contextmenu', 'keydown'])('remembers the items the user touched (%s)', (type) => {
		const { list, root, item } = setup();
		const far = createFarItems();
		const a = item();
		const b = item();
		far.connect(root, list);
		a.querySelector('button')!.dispatchEvent(new Event(type, { bubbles: true }));
		expect(far.isTouched(a)).toBe(true);
		expect(far.isTouched(b)).toBe(false);
	});

	test('stops watching while paused and starts again on resume', () => {
		const { list, root, item } = setup();
		const far = createFarItems();
		const a = item();
		far.observe(a, () => {});
		far.connect(root, list);
		far.pause();
		expect(live()).toHaveLength(0);
		// 止めている間に足した項目も、再開したら見る
		const b = item();
		far.observe(b, () => {});
		far.resume();
		expect(live()).toHaveLength(1);
		expect([...live()[0].observed]).toEqual([a, b]);
	});

	test('does not start a second observer when resumed without a pause', () => {
		// KeepAlive の中では、最初の mount でも onActivated が呼ばれる
		const { list, root, item } = setup();
		const far = createFarItems();
		far.observe(item(), () => {});
		far.connect(root, list);
		far.resume();
		expect(live()).toHaveLength(1);
	});

	test('stops watching an item after it unregisters', () => {
		const { list, root, item } = setup();
		const far = createFarItems();
		const a = item();
		const unobserve = far.observe(a, () => {});
		far.connect(root, list);
		unobserve();
		expect(live()[0].observed.has(a)).toBe(false);
	});

	test('removes the touch listener on disconnect', () => {
		const { list, root, item } = setup();
		const far = createFarItems();
		const a = item();
		far.connect(root, list);
		far.disconnect();
		expect(live()).toHaveLength(0);
		a.dispatchEvent(new Event('click', { bubbles: true }));
		expect(far.isTouched(a)).toBe(false);
	});
});
