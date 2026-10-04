/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterAll, beforeAll, beforeEach, describe, expect, test, vi } from 'vitest';

const reconnect = vi.fn();
const heartbeat = vi.fn();

vi.mock('@/i.js', () => ({ $i: { token: 'TOKEN' } }));
vi.mock('@@/js/config.js', () => ({ wsOrigin: 'wss://misskey.test' }));
vi.mock('misskey-js', () => ({
	Stream: class {
		public reconnect = reconnect;
		public heartbeat = heartbeat;
	},
}));

// 実装側と同じ値。**import しない** — import すると実装側の定数を変える変異に
// テストが追随してしまい、閾値を検査できなくなる。
const HEART_BEAT_INTERVAL = 1000 * 60;

function setVisibilityState(state: 'visible' | 'hidden'): void {
	Object.defineProperty(window.document, 'visibilityState', {
		configurable: true,
		get: () => state,
	});
}

function changeVisibility(state: 'visible' | 'hidden'): void {
	setVisibilityState(state);
	window.document.dispatchEvent(new Event('visibilitychange'));
}

// 時計だけ進める。タイマーは走らせないので、モジュール内部の heartbeat 用
// setTimeout は発火しない。バックグラウンドで凍っていた実機と同じ状態になる。
function elapse(ms: number): void {
	vi.setSystemTime(Date.now() + ms);
}

describe('useStream', () => {
	beforeAll(async () => {
		vi.useFakeTimers();
		setVisibilityState('visible');

		// useStream() 自体は冪等 (`if (stream) return stream;`) なので何度呼んでも
		// リスナーは増えない。増えるのは **モジュールを再 import したとき** で、
		// 古いモジュールのリスナーが同じ mock を叩いて数が合わなくなる。
		// そのため vi.resetModules() は使わず、import は 1 回だけにしてある。
		// 起動時点で hidden だった場合の検査は別ファイルへ分けてある
		// (vitest はファイル単位で隔離するため)。
		const { useStream } = await import('@/stream.js');
		useStream();
	});

	afterAll(() => {
		vi.useRealTimers();
	});

	beforeEach(() => {
		reconnect.mockClear();
		heartbeat.mockClear();
	});

	test('しばらくバックグラウンドに置かれてから復帰したら張り直す', () => {
		changeVisibility('hidden');
		elapse(20 * 1000);
		changeVisibility('visible');

		expect(reconnect).toHaveBeenCalledTimes(1);
		expect(heartbeat).not.toHaveBeenCalled();
	});

	test('閾値に届かない切り替えでは張り直さない', () => {
		changeVisibility('hidden');
		elapse(19 * 1000);
		changeVisibility('visible');

		expect(reconnect).not.toHaveBeenCalled();
	});

	test('heartbeat は前回から間隔が空いているときだけ送る', () => {
		// 前回送信時刻を実測値に揃える。初期値 0 のままだと Date.now() との差が
		// 数十年になり、HEART_BEAT_INTERVAL がいくつでも throttle を素通りする
		// ので、以降の検査が何も見ていない状態になる。
		elapse(HEART_BEAT_INTERVAL * 2);
		changeVisibility('hidden');
		elapse(1000);
		changeVisibility('visible');
		expect(heartbeat).toHaveBeenCalledTimes(1);

		// 送った直後は間隔が空いていないので送らない。
		heartbeat.mockClear();
		changeVisibility('hidden');
		elapse(1000);
		changeVisibility('visible');
		expect(heartbeat).not.toHaveBeenCalled();
		expect(reconnect).not.toHaveBeenCalled();

		// 間隔が空けばまた送る。
		elapse(HEART_BEAT_INTERVAL);
		changeVisibility('hidden');
		elapse(1000);
		changeVisibility('visible');
		expect(heartbeat).toHaveBeenCalledTimes(1);
	});

	test('復帰したあとに visible が重ねて来ても張り直さない', () => {
		// まず短時間だけ隠れて復帰する。
		changeVisibility('hidden');
		elapse(1000);
		changeVisibility('visible');
		expect(reconnect).not.toHaveBeenCalled();

		// 隠れずに visible がもう一度来る。hiddenAt を戻していないと、
		// 実際には隠れていないのに「長時間隠れていた」と誤判定して張り直す。
		elapse(30 * 1000);
		changeVisibility('visible');

		expect(reconnect).not.toHaveBeenCalled();
	});
});
