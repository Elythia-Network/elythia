/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { createReconnectResync, paginatorResyncOptions, NOTIFICATION_RESYNC_PARAMS } from '@/utility/reconnect-resync.js';

// **既定値とは別の数にしてある。** 同じ値にすると「注入した値を使っているのか、
// 既定値にたまたま一致しているのか」がテストから区別できない。
const INTERVAL = 7_000;

function setup(over: {
	jitter?: number;
	holdSleep?: boolean;
	holdResync?: boolean;
	random?: () => number;
	newestId?: () => string | null | undefined;
	initialDelay?: number;
} = {}) {
	let clock = 1_000_000;
	let visible = true;
	let resumed = false;
	// timer は手で発火させる。本物に任せると、どの時点で走ったかをテストが
	// 決められない。
	const scheduled: Array<{ fn: () => void; ms: number; cancelled: boolean }> = [];
	let onVisibleCallback: (() => void) | null = null;
	let unsubscribed = false;
	let newest: string | null = 'n1';
	let can = true;
	let generation = 0;
	const sleepCalls: number[] = [];
	const resyncArgs: Array<string | null> = [];
	let newestCalls = 0;
	// **resolver は配列で持つ。** 単一変数だと 2 本目の待ちが 1 本目の resolver を
	// 上書きし、多重実行の変異がアサーションではなくタイムアウトで落ちる
	// (診断が原因を指さないうえ、testTimeout の変更ひとつで空虚になる)。
	const sleepReleases: Array<() => void> = [];
	const resyncReleases: Array<() => void> = [];
	const resync = vi.fn((sinceId: string | null) => {
		resyncArgs.push(sinceId);
		if (over.holdResync) return new Promise<void>(resolve => { resyncReleases.push(resolve); });
		return Promise.resolve();
	});
	const r = createReconnectResync({
		getNewestId: () => {
			newestCalls++;
			return over.newestId ? over.newestId() : newest;
		},
		getGeneration: () => generation,
		canResync: () => can,
		resync,
		interval: INTERVAL,
		jitter: over.jitter ?? 0,
		now: () => clock,
		// **既定値 (5 秒) とも間隔とも別の数にしてある** (理由はファイル冒頭)。
		initialDelay: over.initialDelay ?? 3_000,
		resumeInterval: 500,
		random: over.random ?? (() => 0),
		schedule: (fn, ms) => {
			const entry = { fn, ms, cancelled: false };
			scheduled.push(entry);
			return () => { entry.cancelled = true; };
		},
		isVisible: () => visible,
		onVisible: (fn) => {
			onVisibleCallback = fn;
			return () => { unsubscribed = true; };
		},
		resumedRecently: () => resumed,
		sleep: (ms: number) => {
			sleepCalls.push(ms);
			if (over.holdSleep) return new Promise<void>(resolve => { sleepReleases.push(resolve); });
			return Promise.resolve();
		},
	});
	return {
		r,
		resync,
		resyncArgs,
		sleepCalls,
		newestCalls: () => newestCalls,
		advance: (ms: number) => { clock += ms; },
		setNewest: (id: string) => { newest = id; },
		setCan: (v: boolean) => { can = v; },
		bumpGeneration: () => { generation++; },
		releaseSleep: () => { for (const f of sleepReleases.splice(0)) f(); },
		releaseResync: () => { for (const f of resyncReleases.splice(0)) f(); },
		/** Settles only the oldest outstanding resync. */
		releaseFirstResync: () => { resyncReleases.shift()?.(); },
		setVisible: (v: boolean) => { visible = v; },
		setResumed: (v: boolean) => { resumed = v; },
		/** Live (not cancelled, not yet fired) timers. */
		liveTimers: () => scheduled.filter(x => !x.cancelled),
		/** Fires every live timer once and waits for the runs they start. */
		fireTimers: async () => {
			const due = scheduled.splice(0).filter(x => !x.cancelled);
			for (const x of due) x.fn();
			// run() は async なので、中の await を流しきる。
			for (let i = 0; i < 10; i++) await Promise.resolve();
		},
		becomeVisible: () => {
			visible = true;
			onVisibleCallback?.();
		},
		isUnsubscribed: () => unsubscribed,
	};
}

describe('createReconnectResync', () => {
	test('does not run on the first connection after mount', async () => {
		const { r, resync } = setup();
		await r.onConnected();
		expect(resync).not.toHaveBeenCalled();
	});

	test('runs once the interval has passed', async () => {
		const { r, resync, advance } = setup();
		advance(INTERVAL);
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(1);
	});

	test('skips a reconnection that arrives within the interval', async () => {
		const { r, resync, advance } = setup();
		advance(INTERVAL);
		await r.onConnected();
		advance(INTERVAL - 1);
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(1);
	});

	test('reads the cursor when the connection opens, not after the jitter', async () => {
		// **この機能の核心。** `_connected_` の時点ではチャンネルの張り直しが
		// まだなので手持ちの最新 = 切断前の最新。ジッターを待ってから読むと、
		// その間に流れ込んだぶんだけ起点が進み、埋めたい穴を飛び越える。
		const { r, resyncArgs, advance, setNewest, releaseSleep } = setup({ jitter: 10_000, holdSleep: true });
		advance(INTERVAL);
		const pending = r.onConnected();
		setNewest('n9');
		releaseSleep();
		await pending;
		expect(resyncArgs).toEqual(['n1']);
	});

	test('does not capture a cursor while the list is being replaced', async () => {
		// `init()` / `reload()` の最中はここを通る。控えてしまうと「一覧が空」と
		// いう誤った起点が、入れ替えを跨いで生き残る。
		const { r, resync, advance, setCan } = setup({ newestId: () => undefined });
		setCan(false);
		advance(INTERVAL);
		await r.onConnected();
		setCan(true);
		await r.retry();
		expect(resync).not.toHaveBeenCalled();
	});

	test('captures a null cursor when the list is genuinely empty', async () => {
		// 空の一覧にも穴は空く (バッジだけ増えて中身が出ない)。呼び出し側が
		// 「最初から取る」形に倒せるよう、控えたうえで null を渡す。
		const { r, resyncArgs, advance } = setup({ newestId: () => null });
		advance(INTERVAL);
		await r.onConnected();
		expect(resyncArgs).toEqual([null]);
	});

	test('does not run on a stale cursor when the list is replaced and reconnected mid-jitter', async () => {
		// ジッターを待つ間に `onConnected` が走ると控えは新しいものに差し替わる。
		// 飛行中の run が `pending` の世代を見ると「一致」してしまい、古い起点の
		// まま投げる (世代機構が防ごうとしているものそのもの)。
		const { r, resyncArgs, advance, setNewest, releaseSleep, bumpGeneration } = setup({ jitter: 10_000, holdSleep: true });
		advance(INTERVAL);
		const first = r.onConnected();
		bumpGeneration();
		setNewest('n9');
		const second = r.onConnected();
		releaseSleep();
		await first;
		await second;
		expect(resyncArgs).toEqual([]);
		const third = r.retry();
		releaseSleep();
		await third;
		expect(resyncArgs).toEqual(['n9']);
	});

	test('recovers on the next connection after the list was replaced', async () => {
		// **「捨てた」は「走らない」からは観測できない。** 捨て損ねると、以後
		// どの `_connected_` も「控えはあるが世代が合わない」で回り続け、
		// 再同期が二度と走らなくなる (リロード 1 回で機能が死ぬ)。
		const { r, resync, resyncArgs, advance, setCan, setNewest, bumpGeneration } = setup();
		setCan(false);
		advance(INTERVAL);
		await r.onConnected();
		bumpGeneration();
		setCan(true);
		await r.retry();
		expect(resync).not.toHaveBeenCalled();
		setNewest('n9');
		await r.onConnected();
		expect(resyncArgs).toEqual(['n9']);
	});

	test('recovers on the next connection after the list was replaced mid-jitter', async () => {
		const { r, resyncArgs, advance, setNewest, releaseSleep, bumpGeneration } = setup({ jitter: 10_000, holdSleep: true });
		advance(INTERVAL);
		const first = r.onConnected();
		bumpGeneration();
		releaseSleep();
		await first;
		setNewest('n9');
		const second = r.onConnected();
		releaseSleep();
		await second;
		expect(resyncArgs).toEqual(['n9']);
	});

	test('captures a fresh cursor when the held one is stale', async () => {
		// 古い控えを捨てるだけで終わると、その接続で生じた穴を誰も埋めない。
		const { r, resyncArgs, advance, setCan, setNewest, bumpGeneration } = setup();
		setCan(false);
		advance(INTERVAL);
		await r.onConnected();
		bumpGeneration();
		setNewest('n9');
		setCan(true);
		await r.onConnected();
		expect(resyncArgs).toEqual(['n9']);
	});

	test('collapses reconnections that arrive while the fetch is in flight', async () => {
		// **待ちは sleep ではなく resync 側で作る。** sleep だけを止めると、
		// `await options.resync(...)` の await を外す変異が素通りする
		// (fetch 区間が無防備になる)。
		const { r, resync, resyncArgs, advance, releaseResync, setNewest } = setup({ holdResync: true });
		advance(INTERVAL);
		const first = r.onConnected();
		advance(INTERVAL * 10);
		setNewest('n2');
		const second = r.onConnected();
		await second;
		// 飛行中は並べない。
		expect(resync).toHaveBeenCalledTimes(1);
		releaseResync();
		await first;
		// **飛行中に来た接続の穴は、終わった時点で埋める** (#3195)。以前はここで
		// 捨てており、次の接続まで誰も拾わなかった。
		expect(resyncArgs).toEqual(['n1', 'n2']);
		releaseResync();
	});

	test('keeps the cursor it captured when it declines up front', async () => {
		// **見送った回も起点は控える。** その場で読み直すと、見送っている間に
		// 届いたぶんだけ起点が進み、埋めたい穴を飛び越える (裏タブで再接続 →
		// 裏で通知が届く → 表に戻って呼び直す、がこの経路)。
		const { r, resyncArgs, advance, setCan, setNewest } = setup();
		setCan(false);
		advance(INTERVAL);
		await r.onConnected();
		setNewest('n9');
		setCan(true);
		await r.retry();
		expect(resyncArgs).toEqual(['n1']);
	});

	test('does not read the cursor after dispose', async () => {
		const { r, newestCalls, advance } = setup();
		r.dispose();
		advance(INTERVAL);
		await r.onConnected();
		expect(newestCalls()).toBe(0);
	});

	test('does not even start waiting on retry after dispose', async () => {
		const { r, sleepCalls, advance, setCan } = setup({ jitter: 10_000 });
		setCan(false);
		advance(INTERVAL);
		await r.onConnected();
		setCan(true);
		r.dispose();
		await r.retry();
		expect(sleepCalls).toEqual([]);
	});

	test('does not overwrite a cursor it is still holding', async () => {
		// 見送っている間に `_connected_` がもう一度来ても、穴の手前側は最初の
		// 切断の時点。上書きすると起点が進む。
		const { r, resyncArgs, advance, setCan, setNewest } = setup();
		setCan(false);
		advance(INTERVAL);
		await r.onConnected();
		setNewest('n9');
		await r.onConnected();
		setCan(true);
		await r.retry();
		expect(resyncArgs).toEqual(['n1']);
	});

	test('does not start waiting when the list was replaced', async () => {
		const { r, sleepCalls, advance, setCan, bumpGeneration } = setup({ jitter: 10_000 });
		setCan(false);
		advance(INTERVAL);
		await r.onConnected();
		bumpGeneration();
		setCan(true);
		await r.retry();
		expect(sleepCalls).toEqual([]);
	});

	test('does not start waiting when it declines up front', async () => {
		const { r, sleepCalls, advance, setCan } = setup({ jitter: 10_000 });
		setCan(false);
		advance(INTERVAL);
		await r.onConnected();
		expect(sleepCalls).toEqual([]);
	});

	test('retry does nothing when no connection was ever made', async () => {
		// 切断が無かったのに撃つと、通知一覧では取得そのものが既読化を伴う。
		const { r, resync, advance } = setup();
		advance(INTERVAL);
		await r.retry();
		expect(resync).not.toHaveBeenCalled();
	});

	test('drops the cursor when the list was replaced', async () => {
		const { r, resync, advance, setCan, bumpGeneration } = setup();
		setCan(false);
		advance(INTERVAL);
		await r.onConnected();
		bumpGeneration();
		setCan(true);
		await r.retry();
		expect(resync).not.toHaveBeenCalled();
	});

	test('drops the cursor when the list is replaced while waiting out the jitter', async () => {
		const { r, resync, advance, releaseSleep, bumpGeneration } = setup({ jitter: 10_000, holdSleep: true });
		advance(INTERVAL);
		const pending = r.onConnected();
		bumpGeneration();
		releaseSleep();
		await pending;
		expect(resync).not.toHaveBeenCalled();
	});

	test('keeps the cursor when it declines after the jitter', async () => {
		const { r, resync, resyncArgs, advance, setCan, setNewest, releaseSleep } = setup({ jitter: 10_000, holdSleep: true });
		advance(INTERVAL);
		const pending = r.onConnected();
		setCan(false);
		releaseSleep();
		await pending;
		expect(resync).not.toHaveBeenCalled();
		// 間隔も消費していないので、時間を進めずに拾い直せる。
		setNewest('n9');
		setCan(true);
		const again = r.retry();
		releaseSleep();
		await again;
		expect(resyncArgs).toEqual(['n1']);
	});

	test('does not consume the interval when it declines up front', async () => {
		// 見送った回を「試行した」ことにすると、条件が整った瞬間に呼び直しても
		// 待たされる (裏タブから戻ったときがこれ)。
		const { r, resync, advance, setCan } = setup();
		setCan(false);
		advance(INTERVAL);
		await r.onConnected();
		expect(resync).not.toHaveBeenCalled();
		setCan(true);
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(1);
	});

	test('stops when the condition changes while waiting out the jitter', async () => {
		const { r, resync, advance, setCan, releaseSleep } = setup({ jitter: 10_000, holdSleep: true });
		advance(INTERVAL);
		const pending = r.onConnected();
		setCan(false);
		releaseSleep();
		await pending;
		expect(resync).not.toHaveBeenCalled();
	});

	test('does not retry immediately after a failure', async () => {
		// 失敗しても「試行した」ことは記録する。記録しないと、落ちているサーバーへ
		// 再接続のたびに投げ続ける。
		const { r, resync, advance } = setup();
		resync.mockRejectedValueOnce(new Error('boom'));
		advance(INTERVAL);
		await r.onConnected();
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(1);
	});

	test('keeps the full interval after a failure', async () => {
		// 失敗時に間隔を詰める仕組みは持たない (`fetchNewer` が reject しないので
		// 失敗を観測できない)。成功時と同じ間隔になることを固定する。
		const { r, resync, advance } = setup();
		resync.mockRejectedValueOnce(new Error('boom'));
		advance(INTERVAL);
		await r.onConnected();
		advance(INTERVAL - 1);
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(1);
		advance(1);
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(2);
	});

	test('keeps the full interval after a success', async () => {
		const { r, resync, advance } = setup();
		advance(INTERVAL);
		await r.onConnected();
		advance(INTERVAL - 1);
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(1);
	});

	test('does not run after dispose', async () => {
		const { r, resync, advance } = setup();
		r.dispose();
		advance(INTERVAL);
		await r.onConnected();
		expect(resync).not.toHaveBeenCalled();
	});

	test('does not even start waiting after dispose', async () => {
		// 入口で弾いていないと、ジッター後のガードで止まるまでに無駄な待ちが走る。
		const { r, resync, sleepCalls, advance } = setup({ jitter: 10_000 });
		r.dispose();
		advance(INTERVAL);
		await r.onConnected();
		expect(sleepCalls).toEqual([]);
		expect(resync).not.toHaveBeenCalled();
	});

	test('does not run when disposed while waiting out the jitter', async () => {
		const { r, resync, advance, releaseSleep } = setup({ jitter: 10_000, holdSleep: true });
		advance(INTERVAL);
		const pending = r.onConnected();
		r.dispose();
		releaseSleep();
		await pending;
		expect(resync).not.toHaveBeenCalled();
	});

	test('waits a jittered delay before running', async () => {
		const { r, sleepCalls, advance } = setup({ jitter: 10_000, random: () => 0.25 });
		advance(INTERVAL);
		await r.onConnected();
		expect(sleepCalls).toEqual([2_500]);
	});

	test('does not sleep when the jitter is zero', async () => {
		const { r, sleepCalls, advance } = setup({ jitter: 0 });
		advance(INTERVAL);
		await r.onConnected();
		expect(sleepCalls).toEqual([]);
	});
});

/**
 * **本番はオプションを 1 つも渡さない** (`getNewestId` / `canResync` / `resync`
 * を除く)。挙動は既定値が 100% 決めるので、注入した値でしか検査しないと既定値を
 * どう壊しても緑のままになる (実測で 6 形すべてが素通りした)。
 *
 * **`await p` の前に必ず余裕を持ってタイマーを進める。** 想定より長く待つ変異で
 * promise が解決せず、アサーションではなくタイムアウトで落ちるのを避ける
 * (診断が原因を指さないうえ、testTimeout ひとつで空虚になる)。
 */
describe('createReconnectResync の既定値', () => {
	const BASE = 1_700_000_000_000;
	// 既定ジッターの上限 (10 秒) より十分長く取る。
	const DRAIN = 60_000;

	beforeEach(() => {
		vi.useFakeTimers();
		vi.setSystemTime(BASE);
	});

	afterEach(() => {
		vi.useRealTimers();
		vi.restoreAllMocks();
	});

	// 既定値のインスタンスは本物の visibilitychange を購読するので、テストごとに
	// 外す (残すと後続のテストの dispatch で過去のインスタンスまで走る)。
	const created: Array<{ dispose: () => void }> = [];
	afterEach(() => {
		for (const x of created.splice(0)) x.dispose();
	});

	function defaults() {
		const resync = vi.fn((_sinceId: string | null) => Promise.resolve() as unknown);
		const r = createReconnectResync({ getNewestId: () => 'n1', resync });
		created.push(r);
		return { resync, r };
	}

	async function run(r: { onConnected: () => Promise<void> }) {
		const p = r.onConnected();
		await vi.advanceTimersByTimeAsync(DRAIN);
		await p;
	}

	test('does not run on the first connection', async () => {
		vi.spyOn(Math, 'random').mockReturnValue(0);
		const { r, resync } = defaults();
		await run(r);
		expect(resync).not.toHaveBeenCalled();
	});

	test('ignores connections within 5 seconds of mount', async () => {
		vi.spyOn(Math, 'random').mockReturnValue(0);
		const { r, resync } = defaults();
		vi.setSystemTime(BASE + 4_999);
		await run(r);
		expect(resync).not.toHaveBeenCalled();
	});

	test('runs a connection 5 seconds after mount without waiting 30 seconds', async () => {
		vi.spyOn(Math, 'random').mockReturnValue(0);
		const { r, resync } = defaults();
		vi.setSystemTime(BASE + 5_000);
		const p = r.onConnected();
		await vi.advanceTimersByTimeAsync(0);
		await p;
		expect(resync).toHaveBeenCalledTimes(1);
	});

	test('waits 30 seconds between runs, then runs the held connection', async () => {
		vi.spyOn(Math, 'random').mockReturnValue(0);
		const { r, resync } = defaults();
		vi.setSystemTime(BASE + 5_000);
		const p1 = r.onConnected();
		await vi.advanceTimersByTimeAsync(0);
		await p1;
		expect(resync).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(10_000);
		const p2 = r.onConnected();
		await vi.advanceTimersByTimeAsync(0);
		await p2;
		expect(resync).toHaveBeenCalledTimes(1);
		// 前回 (BASE + 5 秒) から 30 秒 = BASE + 35 秒で走る。
		await vi.advanceTimersByTimeAsync(19_999);
		expect(resync).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(1);
		// 走らせ直しの中の既定ジッター (random = 0) が timer を 1 段挟む。fake timers は
		// 発火の中で張った 0ms の timer を、時刻を進めないと流さない。
		await vi.advanceTimersByTimeAsync(1);
		expect(resync).toHaveBeenCalledTimes(2);
		r.dispose();
	});

	test('jitters up to 10 seconds', async () => {
		vi.spyOn(Math, 'random').mockReturnValue(0.5);
		const { r, resync } = defaults();
		vi.setSystemTime(BASE + 30_000);
		const p = r.onConnected();
		await vi.advanceTimersByTimeAsync(4_999);
		expect(resync).not.toHaveBeenCalled();
		await vi.advanceTimersByTimeAsync(1);
		// **`await p` より前に見る。** 後ろに置くと、もっと長く待つ変異でも
		// 「最終的には呼ばれた」で通ってしまう。
		expect(resync).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(DRAIN);
		await p;
	});

	test('keeps the 30 second interval after a failure', async () => {
		vi.spyOn(Math, 'random').mockReturnValue(0);
		const { r, resync } = defaults();
		resync.mockRejectedValueOnce(new Error('boom'));
		vi.setSystemTime(BASE + 30_000);
		const p1 = r.onConnected();
		await vi.advanceTimersByTimeAsync(0);
		await p1;
		expect(resync).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(1_000);
		const p2 = r.onConnected();
		await vi.advanceTimersByTimeAsync(0);
		await p2;
		await vi.advanceTimersByTimeAsync(28_999);
		expect(resync).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(1);
		// 走らせ直しの中の既定ジッター (random = 0) が timer を 1 段挟む。fake timers は
		// 発火の中で張った 0ms の timer を、時刻を進めないと流さない。
		await vi.advanceTimersByTimeAsync(1);
		expect(resync).toHaveBeenCalledTimes(2);
		r.dispose();
	});

	// **モジュールで持つ「表示に戻った時刻」を書き換えるので、時刻を過去へずらして
	// から行う。** 後続のテストが BASE で走ったとき「戻った直後」と判定されないように。
	test('does not run while hidden, then runs without jitter once visible', async () => {
		const PAST = BASE - 1_000_000_000;
		vi.setSystemTime(PAST);
		vi.spyOn(Math, 'random').mockReturnValue(0.99);
		const { r, resync } = defaults();
		const setVisibility = (state: DocumentVisibilityState) => {
			Object.defineProperty(window.document, 'visibilityState', { value: state, configurable: true });
		};
		try {
			setVisibility('hidden');
			vi.setSystemTime(PAST + 5_000);
			const p = r.onConnected();
			await vi.advanceTimersByTimeAsync(DRAIN);
			await p;
			expect(resync).not.toHaveBeenCalled();
			setVisibility('visible');
			window.document.dispatchEvent(new Event('visibilitychange'));
			// 同期では走らない (呼び出し側の watch を先に流す)。
			expect(resync).not.toHaveBeenCalled();
			// ジッター (0.99 × 10 秒) を待たずに走る。
			await vi.advanceTimersByTimeAsync(10);
			expect(resync).toHaveBeenCalledTimes(1);
		} finally {
			r.dispose();
			// happy-dom の既定 (getter) へ戻す。
			delete (window.document as { visibilityState?: unknown }).visibilityState;
		}
	});

	// 上と同じく、時刻を過去へずらしてから表示に戻す。
	test('skips the jitter for 10 seconds after becoming visible and spaces runs by 2 seconds', async () => {
		const PAST = BASE - 2_000_000_000;
		vi.setSystemTime(PAST);
		vi.spyOn(Math, 'random').mockReturnValue(0.99);
		const { r, resync } = defaults();
		window.document.dispatchEvent(new Event('visibilitychange'));
		const connect = async () => {
			const p = r.onConnected();
			await vi.advanceTimersByTimeAsync(1);
			await p;
		};
		vi.setSystemTime(PAST + 5_000);
		await connect();
		// ジッター (9.9 秒) を待たずに走った。
		expect(resync).toHaveBeenCalledTimes(1);
		vi.setSystemTime(PAST + 6_998);
		await connect();
		expect(resync).toHaveBeenCalledTimes(1);
		// 前回 (PAST + 5 秒) から 2 秒で走る。
		await vi.advanceTimersByTimeAsync(10);
		expect(resync).toHaveBeenCalledTimes(2);
		// ちょうど 10 秒で「戻った直後」は終わる。30 秒の間隔に戻るので、2 秒では
		// 走らない (窓を広げる変異はここで 2 秒後に走る)。
		vi.setSystemTime(PAST + 10_000);
		const p = r.onConnected();
		await vi.advanceTimersByTimeAsync(3_000);
		expect(resync).toHaveBeenCalledTimes(2);
		r.dispose();
		await p;
	});
});

/**
 * #3195: 見送った回を捨てない / 裏では投げない / 復帰直後は待たない。
 *
 * 実機 (iOS 18.6.2 の PWA) で、裏で自動リロードされたページに 20 秒後に戻ると、
 * 張り直しの穴埋めが「生成から 30 秒以内」で見送られ、その後二度と走らずに
 * タイムラインが古いまま残った。
 */
describe('createReconnectResync (#3195)', () => {
	test('runs a connection right after the initial delay without waiting out the interval', async () => {
		// **これが実機で踏んだ形。** 以前は生成時刻を「前回」に数えていたので、
		// 間隔 (ここでは 7 秒) が明けるまで見送った。
		const { r, resync, newestCalls, advance } = setup();
		advance(2_999);
		await r.onConnected();
		// mount 直後の接続は起点すら控えない (init が取っている)。
		expect(newestCalls()).toBe(0);
		advance(1);
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(1);
	});

	test('does not arm a timer for a connection inside the initial delay', async () => {
		// 控えないので、期限が明けても init と同じ範囲を取りに行かない。
		const { r, resync, liveTimers, fireTimers } = setup();
		await r.onConnected();
		expect(liveTimers()).toHaveLength(0);
		await fireTimers();
		expect(resync).not.toHaveBeenCalled();
	});

	test('runs a held connection once the interval ends', async () => {
		const { r, resyncArgs, advance, setNewest, liveTimers, fireTimers } = setup();
		advance(INTERVAL);
		await r.onConnected();
		expect(resyncArgs).toEqual(['n1']);
		advance(1_000);
		setNewest('n2');
		await r.onConnected();
		expect(resyncArgs).toEqual(['n1']);
		// 明ける時刻ちょうどに張る。
		expect(liveTimers().map(x => x.ms)).toEqual([INTERVAL - 1_000]);
		setNewest('n3');
		advance(INTERVAL - 1_000);
		await fireTimers();
		// **控えた起点で走る** (走らせ直す時点の最新ではない)。
		expect(resyncArgs).toEqual(['n1', 'n2']);
	});

	test('keeps a single timer for several held connections', async () => {
		const { r, advance, liveTimers } = setup();
		advance(INTERVAL);
		await r.onConnected();
		advance(100);
		await r.onConnected();
		advance(100);
		await r.onConnected();
		expect(liveTimers()).toHaveLength(1);
	});

	test('does not arm a timer when it runs right away', async () => {
		const { r, resync, advance, liveTimers } = setup();
		advance(INTERVAL);
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(1);
		expect(liveTimers()).toHaveLength(0);
	});

	test('does not run while hidden and keeps the cursor for when it becomes visible', async () => {
		const { r, resync, resyncArgs, advance, setVisible, setNewest, becomeVisible, liveTimers, fireTimers } = setup();
		advance(INTERVAL);
		setVisible(false);
		await r.onConnected();
		expect(resync).not.toHaveBeenCalled();
		// 裏にいる間に届いたぶんで起点が進んではいけない。
		setNewest('n2');
		becomeVisible();
		// **同期では走らない。** 呼び出し側の watch (`isPausingUpdate`) より先に
		// 投げると、取れたノートが queue に取り残される。
		expect(resync).not.toHaveBeenCalled();
		expect(liveTimers().map(x => x.ms)).toEqual([0]);
		await fireTimers();
		expect(resyncArgs).toEqual(['n1']);
	});

	test('does not start waiting out the jitter while hidden', async () => {
		// 裏で待ち始めると飛行中の扱いになり、その間に表へ戻った回が弾かれて、
		// 戻ってからもジッターぶん待たされる。
		const { r, sleepCalls, advance, setVisible } = setup({ jitter: 1_000, holdSleep: true });
		advance(INTERVAL);
		setVisible(false);
		await r.onConnected();
		expect(sleepCalls).toEqual([]);
	});

	test('does not consume the interval while hidden', async () => {
		const { r, resync, advance, setVisible, becomeVisible, fireTimers } = setup();
		advance(INTERVAL);
		setVisible(false);
		await r.onConnected();
		advance(1);
		becomeVisible();
		await fireTimers();
		expect(resync).toHaveBeenCalledTimes(1);
	});

	test('declines after the jitter when sent to the background meanwhile', async () => {
		// 実機で起きた形: 待っている間に裏へ戻され、取得が裏で投げられて 1 分半止まった。
		const { r, resync, resyncArgs, advance, setVisible, releaseSleep, becomeVisible, fireTimers } = setup({ jitter: 1_000, holdSleep: true });
		advance(INTERVAL);
		const p = r.onConnected();
		setVisible(false);
		releaseSleep();
		await p;
		expect(resync).not.toHaveBeenCalled();
		becomeVisible();
		const q = fireTimers();
		releaseSleep();
		await q;
		releaseSleep();
		for (let i = 0; i < 10; i++) await Promise.resolve();
		expect(resyncArgs).toEqual(['n1']);
	});

	test('does not wait out the jitter right after the page becomes visible', async () => {
		const { r, resync, sleepCalls, advance, setResumed } = setup({ jitter: 1_000, random: () => 0.9 });
		advance(INTERVAL);
		setResumed(true);
		await r.onConnected();
		expect(sleepCalls).toEqual([]);
		expect(resync).toHaveBeenCalledTimes(1);
	});

	test('still waits out the jitter when the page did not just become visible', async () => {
		// サーバー再起動で全員が同時に繋ぎ直す形。散らす役目は残す。
		const { r, sleepCalls, advance, setResumed } = setup({ jitter: 1_000, random: () => 0.9 });
		advance(INTERVAL);
		setResumed(false);
		await r.onConnected();
		expect(sleepCalls).toEqual([900]);
	});

	test('does nothing on becoming visible without a held cursor', async () => {
		const { r, resync, newestCalls, advance, becomeVisible, fireTimers } = setup();
		advance(INTERVAL);
		becomeVisible();
		await fireTimers();
		expect(resync).not.toHaveBeenCalled();
		expect(newestCalls()).toBe(0);
		// 使わない変数の警告を避けるためだけに参照する。
		expect(r).toBeDefined();
	});

	test('abandons a fetch that spanned a background period and runs again on becoming visible', async () => {
		// 実機の形: 投げた直後に裏へ戻され、iOS に止められた取得が 1.5 分決着
		// しなかった。その間の復帰は飛行中で弾かれ、何も走らなかった。
		const { r, resyncArgs, advance, setVisible, becomeVisible, fireTimers, releaseResync } = setup({ holdResync: true });
		advance(INTERVAL);
		void r.onConnected();
		await Promise.resolve();
		expect(resyncArgs).toEqual(['n1']);
		advance(1_000);
		setVisible(false);
		becomeVisible();
		await fireTimers();
		// **同じ起点で投げ直す** (見捨てた回は控えを捨てていたので戻す)。
		// **間隔も数えない** — 数えると 1 秒前に投げたことになり 30 秒待たされる。
		expect(resyncArgs).toEqual(['n1', 'n1']);
		releaseResync();
	});

	test('does not let an abandoned fetch clear the flight of the one that replaced it', async () => {
		const { r, resync, advance, setVisible, becomeVisible, fireTimers, releaseResync, releaseFirstResync, setNewest } = setup({ holdResync: true });
		advance(INTERVAL);
		void r.onConnected();
		await Promise.resolve();
		setVisible(false);
		becomeVisible();
		await fireTimers();
		expect(resync).toHaveBeenCalledTimes(2);
		// 見捨てた回だけ先に決着させる。後を継いだ回はまだ飛行中。
		releaseFirstResync();
		for (let i = 0; i < 10; i++) await Promise.resolve();
		// ここで来た接続は並べずに控えるだけ (見捨てた回が印を消していれば並ぶ)。
		advance(INTERVAL * 10);
		setNewest('n2');
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(2);
		releaseResync();
	});

	test('does not fetch after waking from a jitter it was abandoned in', async () => {
		const { r, resync, advance, setVisible, becomeVisible, fireTimers, releaseSleep } = setup({ jitter: 1_000, holdSleep: true });
		advance(INTERVAL);
		const p = r.onConnected();
		setVisible(false);
		becomeVisible();
		await fireTimers();
		// 見捨てた回も後を継いだ回もジッターで寝ている。両方起こす。
		releaseSleep();
		await p;
		for (let i = 0; i < 10; i++) await Promise.resolve();
		expect(resync).toHaveBeenCalledTimes(1);
	});

	test('ignores the interval right after the page becomes visible', async () => {
		const { r, resync, advance, setResumed, liveTimers } = setup();
		advance(INTERVAL);
		await r.onConnected();
		advance(1_000);
		setResumed(true);
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(2);
		expect(liveTimers()).toHaveLength(0);
	});

	test('still spaces runs right after the page becomes visible', async () => {
		// 復帰直後に接続が暴れても、張り直しのたびには撃たない。
		const { r, resync, advance, setResumed, liveTimers, fireTimers } = setup();
		advance(INTERVAL);
		setResumed(true);
		await r.onConnected();
		advance(100);
		await r.onConnected();
		expect(resync).toHaveBeenCalledTimes(1);
		expect(liveTimers().map(x => x.ms)).toEqual([400]);
		advance(400);
		await fireTimers();
		expect(resync).toHaveBeenCalledTimes(2);
	});

	test('re-arms a longer timer when the page becomes visible', async () => {
		// 3 周目のレビューで見つかった回帰: 窓の外で張った 30 秒 (ここでは 7 秒) の
		// timer が残り、復帰直後に 2 秒 (ここでは 0.5 秒) で走るはずの回を吸った。
		const { r, resync, advance, setResumed, liveTimers, fireTimers } = setup();
		advance(INTERVAL);
		await r.onConnected();
		advance(100);
		await r.onConnected();
		expect(liveTimers().map(x => x.ms)).toEqual([INTERVAL - 100]);
		setResumed(true);
		await r.onConnected();
		// 古い timer は取り消し、0.5 秒で明ける時刻に張り直す。
		expect(liveTimers().map(x => x.ms)).toEqual([400]);
		advance(400);
		await fireTimers();
		expect(resync).toHaveBeenCalledTimes(2);
	});

	test('does not re-arm to a later time', async () => {
		const { r, advance, setResumed, liveTimers } = setup();
		advance(INTERVAL);
		setResumed(true);
		await r.onConnected();
		advance(100);
		await r.onConnected();
		expect(liveTimers().map(x => x.ms)).toEqual([400]);
		// 窓が切れて 30 秒の判断に戻っても、張ってある早いほうはそのまま。発火時の
		// run() が改めて判断する。
		setResumed(false);
		await r.onConnected();
		expect(liveTimers().map(x => x.ms)).toEqual([400]);
	});

	test('does not overwrite a newer-generation cursor with the one it abandons', async () => {
		// 2 周目のレビューで見つかった回帰: 裏にいる間に一覧が総入れ替えされ、
		// そのあと来た接続が新しい世代で控えた。見捨てた回の古い起点で上書きすると、
		// その控えは世代違いで止まり続け、その接続の穴が二度と埋まらない。
		const { r, resyncArgs, advance, setVisible, becomeVisible, fireTimers, bumpGeneration, setNewest, releaseResync } = setup({ holdResync: true });
		advance(INTERVAL);
		void r.onConnected();
		await Promise.resolve();
		expect(resyncArgs).toEqual(['n1']);
		setVisible(false);
		bumpGeneration();
		setNewest('b');
		await r.onConnected();
		becomeVisible();
		await fireTimers();
		expect(resyncArgs).toEqual(['n1', 'b']);
		releaseResync();
	});

	test('dispose cancels the timer and stops listening for visibility', async () => {
		const { r, resync, advance, liveTimers, fireTimers, isUnsubscribed } = setup();
		advance(INTERVAL);
		await r.onConnected();
		advance(100);
		await r.onConnected();
		expect(liveTimers()).toHaveLength(1);
		r.dispose();
		expect(liveTimers()).toHaveLength(0);
		expect(isUnsubscribed()).toBe(true);
		advance(INTERVAL);
		await fireTimers();
		expect(resync).toHaveBeenCalledTimes(1);
	});
});

/**
 * 判断を `.vue` から出した部分。過去の周回で何度も取り違えた述語 (入れ替え中か
 * どうか / 見送る条件 / どこへ入れるか / どのパラメータを足すか) がここに集まって
 * いる。SFC の中にあるとテストから触れないので、変異させても誰も気付かない。
 */
describe('paginatorResyncOptions', () => {
	function fake(over: Record<string, unknown> = {}) {
		const calls: Array<Record<string, unknown>> = [];
		let reloads = 0;
		const reloadArgs: Array<Record<string, unknown> | undefined> = [];
		const p = {
			fetching: { value: false },
			rateLimited: { value: false },
			error: { value: false },
			generation: 3,
			getNewestId: () => 'n1' as string | null | undefined,
			fetchNewer: (o: Record<string, unknown>) => { calls.push(o); return Promise.resolve(); },
			reload: (rp?: Record<string, unknown>) => { reloads++; reloadArgs.push(rp); return Promise.resolve(); },
			...over,
		};
		return { p: p as Parameters<typeof paginatorResyncOptions>[0], calls, reloadCount: () => reloads, reloadParams: () => reloadArgs };
	}

	test('入れ替え中は「いま決められない」と答える', () => {
		const { p } = fake({ fetching: { value: true } });
		expect(paginatorResyncOptions(p, { toQueue: () => false }).getNewestId()).toBeUndefined();
	});

	test('一覧が空なら null を返す (控えたうえで最初から取らせる)', () => {
		const { p } = fake({ getNewestId: () => undefined });
		expect(paginatorResyncOptions(p, { toQueue: () => false }).getNewestId()).toBeNull();
	});

	test('起点があればそのまま返す', () => {
		const { p } = fake();
		expect(paginatorResyncOptions(p, { toQueue: () => false }).getNewestId()).toBe('n1');
	});

	test('世代は paginator のものを渡す', () => {
		const { p } = fake();
		expect(paginatorResyncOptions(p, { toQueue: () => false }).getGeneration!()).toBe(3);
	});

	test('init / reload 中は見送る', () => {
		const { p } = fake({ fetching: { value: true } });
		expect(paginatorResyncOptions(p, { toQueue: () => false }).canResync!()).toBe(false);
	});

	test('レート制限中は見送る', () => {
		const { p } = fake({ rateLimited: { value: true } });
		expect(paginatorResyncOptions(p, { toQueue: () => false }).canResync!()).toBe(false);
	});

	test('一度も読み込めていない一覧では見送る', () => {
		// `init()` が失敗した一覧に「切断中の穴」は定義できない。
		const { p } = fake({ error: { value: true } });
		expect(paginatorResyncOptions(p, { toQueue: () => false }).canResync!()).toBe(false);
	});

	test('どれでもなければ走る', () => {
		const { p } = fake();
		expect(paginatorResyncOptions(p, { toQueue: () => false }).canResync!()).toBe(true);
	});

	test('通知一覧の穴埋めは既読化しない', async () => {
		// **これが無いと、背景の穴埋めのたびに未読が全部消える。**
		const { p, calls } = fake();
		await paginatorResyncOptions(p, { toQueue: () => false, params: NOTIFICATION_RESYNC_PARAMS }).resync('n1');
		expect(calls).toEqual([{ sinceId: 'n1', toQueue: false, params: { markAsRead: false } }]);
	});

	test('起点が無ければ取り直す', async () => {
		// `fetchNewer` では表現できない (起点を省くと降順で返って並びが壊れ、
		// `'0'` だと最古のページを取る)。空の一覧は失うものが無い。
		const { p, calls, reloadCount } = fake();
		await paginatorResyncOptions(p, { toQueue: () => false }).resync(null);
		expect(reloadCount()).toBe(1);
		expect(calls).toEqual([]);
	});

	test('述語は呼ぶたびに読み直す', async () => {
		// **ここを構築時のスナップショットにすると機能が丸ごと死ぬ。** ヘルパーは
		// SFC の setup で作られ、`Paginator.fetching` は `ref(true)` で初期化されて
		// `init()` は `onMounted` で走る。構築時点の値は必ず `true` なので、
		// `canResync` は永久に false、`getNewestId` は永久に `undefined` になる。
		let queue = false;
		const { p, calls } = fake();
		const o = paginatorResyncOptions(p, { toQueue: () => queue });
		expect(o.canResync!()).toBe(true);
		expect(o.getNewestId()).toBe('n1');

		p.fetching.value = true;
		expect(o.canResync!()).toBe(false);
		expect(o.getNewestId()).toBeUndefined();

		p.fetching.value = false;
		p.rateLimited.value = true;
		expect(o.canResync!()).toBe(false);

		p.rateLimited.value = false;
		p.error.value = true;
		expect(o.canResync!()).toBe(false);

		p.error.value = false;
		p.generation = 9;
		expect(o.getGeneration!()).toBe(9);

		queue = true;
		await o.resync('n1');
		expect(calls.at(-1)!.toQueue).toBe(true);

		// **起点そのものも読み直す。** ここを構築時に固定すると、setup 時点
		// (init 前で items が空) の `undefined` が残り、毎回 `reload()` の枝に倒れる。
		p.getNewestId = () => 'n9';
		expect(o.getNewestId()).toBe('n9');
		p.getNewestId = () => undefined;
		expect(o.getNewestId()).toBeNull();
	});

	test('起点が無ければ取り直すときも追加パラメータを渡す', async () => {
		// `params` が `fetchNewer` の枝にしか効かないと、一覧が空のときだけ
		// 背景の穴埋めが既読化する (`init()` は `markAsRead` 既定 true で撃つ)。
		const { p, reloadParams } = fake();
		await paginatorResyncOptions(p, { toQueue: () => false, params: NOTIFICATION_RESYNC_PARAMS }).resync(null);
		expect(reloadParams()).toEqual([{ markAsRead: false }]);
	});

	test('queue へ積むかは呼び出し側が決める', async () => {
		const { p, calls } = fake();
		await paginatorResyncOptions(p, { toQueue: () => true }).resync('n1');
		expect(calls[0].toQueue).toBe(true);
	});

	test('params を渡さないときはキーごと付けない', async () => {
		const { p, calls } = fake();
		await paginatorResyncOptions(p, { toQueue: () => false }).resync('n1');
		expect(calls[0]).not.toHaveProperty('params');
	});
});
