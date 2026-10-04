/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { bindLongPress } from '@/utility/long-press.js';

/**
 * mk-go: リアクションの長押しでメニューを開くための土台 (#2932)。
 *
 * **iOS Safari は `<button>` の長押しで `contextmenu` を発火しない**ので、
 * `@contextmenu` にだけ配線したメニューは iOS から到達できない。タップが別の
 * 動作 (リアクションの付け外し) に取られていると代わりの入口が無くなる。
 *
 * **リスナーの登録順を実物に合わせてある。** Vue は `@click` を patch 中に張り、
 * `bindLongPress` は `onMounted` で張るので、**Vue の方が先**。この順で組まないと
 * `capture: true` を外しても bubble パスでこちらが先に走ってしまい、実装の最も
 * 微妙な部分が無検証になる (初版が実際にそうなっていた)。
 *
 * **タッチの一連の流れをそのまま組む。** 発火後のケースで `touchend` を省くと
 * 「どれだけ長く押していたか」という軸が消え、握り潰しの期限が発火時刻起点でも
 * touchend 起点でも同じ結果になる。初版がそうなっており、**メニューを読んでから
 * 指を離すとリアクションが動く**という一番普通の経路を 2 周にわたって見逃した。
 *
 * **実機での確認は iOS でしかできない。** ここで固定するのは「タイマーが発火する /
 * 取り消される」と「後続の click と contextmenu を握り潰す」という機構だけで、
 * iOS Safari が実際に contextmenu を出さないことは前提であって検証していない。
 */

type Point = { clientX: number; clientY: number };

// happy-dom には TouchEvent のコンストラクタが無いので、必要な形だけ合成する。
//
// **`touches` と `targetTouches` を作り分ける。** 前者は画面全体、後者はこの要素の
// 接触点で、実装が見るのは後者。同じ値にしてしまうと「画面の別の場所に指が触れて
// いるだけで長押しが死ぬ」形を検出できない。
function touchEvent(type: string, target: Point[], all: Point[] = target): Event {
	const ev = new Event(type, { bubbles: true, cancelable: true });
	Object.defineProperty(ev, 'touches', { value: all, configurable: true });
	Object.defineProperty(ev, 'targetTouches', { value: target, configurable: true });
	return ev;
}

function mouseEvent(type: string): Event {
	return new Event(type, { bubbles: true, cancelable: true });
}

describe('bindLongPress', () => {
	let el: HTMLElement;
	let dispose: () => void;
	let fired: number;
	/** Vue の `@click` (= リアクションの付け外し) に相当する。 */
	let clicked: number;
	/** Vue の `@contextmenu` (= メニュー) に相当する。 */
	let contextmenued: number;

	function press(x = 0, y = 0) {
		el.dispatchEvent(touchEvent('touchstart', [{ clientX: x, clientY: y }]));
	}

	beforeEach(() => {
		vi.useFakeTimers();
		fired = 0;
		clicked = 0;
		contextmenued = 0;
		el = window.document.createElement('button');
		window.document.body.appendChild(el);
		// **先に張るのが要点** (実物では Vue の patch が先)。
		el.addEventListener('click', () => { clicked++; });
		el.addEventListener('contextmenu', () => { contextmenued++; });
		dispose = bindLongPress(el, () => { fired++; });
	});

	afterEach(() => {
		dispose();
		el.remove();
		vi.useRealTimers();
	});

	describe('発火の条件', () => {
		test('押し続けると発火する', () => {
			press();
			vi.advanceTimersByTime(500);
			expect(fired).toBe(1);
		});

		test('既定の delay より前では発火しない', () => {
			press();
			vi.advanceTimersByTime(499);
			expect(fired).toBe(0);
		});

		test('離すと発火しない', () => {
			press();
			vi.advanceTimersByTime(400);
			el.dispatchEvent(touchEvent('touchend', []));
			vi.advanceTimersByTime(500);
			expect(fired).toBe(0);
		});

		test('touchcancel でも取り消される', () => {
			press();
			el.dispatchEvent(touchEvent('touchcancel', []));
			vi.advanceTimersByTime(500);
			expect(fired).toBe(0);
		});

		test('許容量を超えて動かすと発火しない (スクロール)', () => {
			press(0, 0);
			el.dispatchEvent(touchEvent('touchmove', [{ clientX: 0, clientY: 20 }]));
			vi.advanceTimersByTime(500);
			expect(fired).toBe(0);
		});

		test('許容量の中の揺れでは取り消されない', () => {
			press(0, 0);
			el.dispatchEvent(touchEvent('touchmove', [{ clientX: 3, clientY: 3 }]));
			vi.advanceTimersByTime(500);
			expect(fired).toBe(1);
		});

		test('ちょうど許容量なら取り消されない (境界)', () => {
			press(0, 0);
			el.dispatchEvent(touchEvent('touchmove', [{ clientX: 8, clientY: 0 }]));
			vi.advanceTimersByTime(500);
			expect(fired).toBe(1);
		});

		test('この要素の上で 2 本指なら発火しない (ピンチ)', () => {
			el.dispatchEvent(touchEvent('touchstart', [{ clientX: 0, clientY: 0 }, { clientX: 50, clientY: 50 }]));
			vi.advanceTimersByTime(500);
			expect(fired).toBe(0);
		});

		test('画面の別の場所に指が触れていても発火する', () => {
			// **`touches` ではなく `targetTouches` で数える。** 前者は画面全体の
			// 接触点なので、どこかに指が残っているだけで長押しが黙って死ぬ。
			el.dispatchEvent(touchEvent('touchstart',
				[{ clientX: 0, clientY: 0 }],
				[{ clientX: 0, clientY: 0 }, { clientX: 300, clientY: 500 }]));
			vi.advanceTimersByTime(500);
			expect(fired).toBe(1);
		});

		test('delay と moveTolerance を渡せる', () => {
			dispose();
			dispose = bindLongPress(el, () => { fired++; }, { delay: 100, moveTolerance: 40 });
			press(0, 0);
			el.dispatchEvent(touchEvent('touchmove', [{ clientX: 0, clientY: 20 }]));
			vi.advanceTimersByTime(100);
			expect(fired).toBe(1);
		});
	});

	describe('後続イベントの握り潰し', () => {
		// **この要素の @click は「リアクションの付け外し」。** 握り潰さないと、
		// メニューを開いた指を離した瞬間にリアクションが動く。
		test('長押しの後、指を離したときの click は通らない', () => {
			press();
			vi.advanceTimersByTime(500);
			el.dispatchEvent(touchEvent('touchend', []));
			el.dispatchEvent(mouseEvent('click'));
			expect(fired).toBe(1);
			expect(clicked).toBe(0);
		});

		test('メニューを読んでから指を離しても click は通らない', () => {
			// **期限の起点が touchend でないと抜ける経路。** 発火時刻を起点に
			// すると、押している時間が窓を超えただけでリアクションが動く。
			press();
			vi.advanceTimersByTime(500);
			vi.advanceTimersByTime(5000);
			el.dispatchEvent(touchEvent('touchend', []));
			el.dispatchEvent(mouseEvent('click'));
			expect(fired).toBe(1);
			expect(clicked).toBe(0);
		});

		test('指を離す前に来た click も通らない', () => {
			// 押している間は期限がまだ入っていない (0)。**時間で切らない** —
			// 発火時刻を起点に期限を張ると、長く押しただけでここが抜ける。
			press();
			vi.advanceTimersByTime(500);
			vi.advanceTimersByTime(3000);
			el.dispatchEvent(mouseEvent('click'));
			expect(clicked).toBe(0);
		});

		test('長押ししていない普通のタップの click は通る', () => {
			press();
			vi.advanceTimersByTime(100);
			el.dispatchEvent(touchEvent('touchend', []));
			el.dispatchEvent(mouseEvent('click'));
			expect(fired).toBe(0);
			expect(clicked).toBe(1);
		});

		test('握り潰すのは 1 回だけ (次のタップは通る)', () => {
			press();
			vi.advanceTimersByTime(500);
			el.dispatchEvent(mouseEvent('click'));
			el.dispatchEvent(mouseEvent('click'));
			expect(clicked).toBe(1);
		});

		test('メニュー側をタップして click が来なかった後でも、次のタップは通る', () => {
			press();
			vi.advanceTimersByTime(500);
			// この要素の click は来ないまま次の操作へ。
			press();
			vi.advanceTimersByTime(100);
			el.dispatchEvent(touchEvent('touchend', []));
			el.dispatchEvent(mouseEvent('click'));
			expect(clicked).toBe(1);
		});

		test('指を離してからしばらく経ったクリックは握り潰さない', () => {
			// 長押しの後、この要素の click が来ないまま時間が経った場合
			// (メニュー側を操作した後にマウスやキーボードで触る等)。
			press();
			vi.advanceTimersByTime(500);
			el.dispatchEvent(touchEvent('touchend', []));
			vi.advanceTimersByTime(1000);
			el.dispatchEvent(mouseEvent('click'));
			expect(clicked).toBe(1);
		});

		test('長押しの後、タッチを挟まないマウスの右クリックはメニューが開く', () => {
			// iOS は長押しで contextmenu を出さないので、こちらのフラグは
			// 立ったまま残る。時刻で縛らないと次の右クリックを 1 回食う。
			press();
			vi.advanceTimersByTime(500);
			el.dispatchEvent(touchEvent('touchend', []));
			vi.advanceTimersByTime(60_000);
			el.dispatchEvent(mouseEvent('contextmenu'));
			expect(contextmenued).toBe(1);
		});

		test('長押しの後に 2 本目の指が触れても握り潰しは解けない', () => {
			press();
			vi.advanceTimersByTime(500);
			el.dispatchEvent(touchEvent('touchstart', [{ clientX: 0, clientY: 0 }, { clientX: 50, clientY: 50 }]));
			el.dispatchEvent(touchEvent('touchend', []));
			el.dispatchEvent(mouseEvent('click'));
			expect(clicked).toBe(0);
		});

		test('2 本指で始めた次の操作に前回の握り潰しが持ち越されない', () => {
			// **状態を落とすのは「新しい操作の始まり」。** 1 本指かどうかで判定
			// すると、2 本指で始めた操作に前回の armed が残り、普通のタップが食われる。
			press();
			vi.advanceTimersByTime(500);
			el.dispatchEvent(touchEvent('touchend', []));
			// メニュー側をタップして閉じたので、この要素の click は来ない。
			vi.advanceTimersByTime(5000);
			el.dispatchEvent(touchEvent('touchstart', [{ clientX: 0, clientY: 0 }, { clientX: 50, clientY: 50 }]));
			vi.advanceTimersByTime(80);
			el.dispatchEvent(touchEvent('touchend', []));
			el.dispatchEvent(mouseEvent('click'));
			expect(clicked).toBe(1);
		});

		test('2 本指の片方を離しただけでは操作は終わらない', () => {
			// 無条件に touching を false にすると、その後の contextmenu が
			// タッチ由来と判定されず、後続の click を握り潰しそこねる。
			el.dispatchEvent(touchEvent('touchstart', [{ clientX: 0, clientY: 0 }]));
			el.dispatchEvent(touchEvent('touchstart',
				[{ clientX: 0, clientY: 0 }, { clientX: 50, clientY: 50 }]));
			el.dispatchEvent(touchEvent('touchend', [{ clientX: 0, clientY: 0 }]));
			vi.advanceTimersByTime(1500);
			el.dispatchEvent(mouseEvent('contextmenu'));
			el.dispatchEvent(touchEvent('touchend', []));
			el.dispatchEvent(mouseEvent('click'));
			expect(contextmenued).toBe(1);
			expect(clicked).toBe(0);
		});

		test('touchend の後に来た contextmenu でも、その後の click は 1 回しか食わない', () => {
			// ここで期限を入れないと armed が期限無しで立ちっぱなしになる。
			press();
			vi.advanceTimersByTime(300);
			el.dispatchEvent(touchEvent('touchend', []));
			el.dispatchEvent(mouseEvent('contextmenu'));
			vi.advanceTimersByTime(60_000);
			el.dispatchEvent(mouseEvent('click'));
			expect(contextmenued).toBe(1);
			expect(clicked).toBe(1);
		});

		test('delay を長くしてもブラウザ先行の click 抑止は効く', () => {
			// 「タッチ由来か」を時刻の差だけで見ると、delay が窓を超えた時点で
			// 黙って無効になる。指が触れているかで見る。
			dispose();
			dispose = bindLongPress(el, () => { fired++; }, { delay: 3000 });
			press();
			vi.advanceTimersByTime(2000);
			el.dispatchEvent(mouseEvent('contextmenu'));
			el.dispatchEvent(touchEvent('touchend', []));
			el.dispatchEvent(mouseEvent('click'));
			expect(contextmenued).toBe(1);
			expect(clicked).toBe(0);
		});

		test('こちらが先に撃ったら contextmenu は通らない (二重に開かない)', () => {
			press();
			vi.advanceTimersByTime(500);
			el.dispatchEvent(mouseEvent('contextmenu'));
			expect(fired).toBe(1);
			expect(contextmenued).toBe(0);
		});

		test('ブラウザが先に contextmenu を撃ったら、こちらは撃たない', () => {
			press();
			vi.advanceTimersByTime(300);
			el.dispatchEvent(mouseEvent('contextmenu'));
			vi.advanceTimersByTime(500);
			expect(contextmenued).toBe(1);
			expect(fired).toBe(0);
		});

		test('ブラウザが先に撃った場合も、後続の click は握り潰す', () => {
			// Android Chrome の経路。contextmenu を出したブラウザが tap を抑止
			// しない実装に当たると、メニューの裏でリアクションが動く。
			press();
			vi.advanceTimersByTime(300);
			el.dispatchEvent(mouseEvent('contextmenu'));
			vi.advanceTimersByTime(3000);
			el.dispatchEvent(touchEvent('touchend', []));
			el.dispatchEvent(mouseEvent('click'));
			expect(contextmenued).toBe(1);
			expect(clicked).toBe(0);
		});

		test('デスクトップの右クリックは後続の click を握り潰さない', () => {
			// 触っていないので、contextmenu はタッチ由来ではない。
			el.dispatchEvent(mouseEvent('contextmenu'));
			el.dispatchEvent(mouseEvent('click'));
			expect(contextmenued).toBe(1);
			expect(clicked).toBe(1);
		});
	});

	test('dispose するとリスナーが外れる', () => {
		dispose();
		press();
		vi.advanceTimersByTime(500);
		el.dispatchEvent(touchEvent('touchend', []));
		el.dispatchEvent(mouseEvent('click'));
		expect(fired).toBe(0);
		expect(clicked).toBe(1);
	});
});
