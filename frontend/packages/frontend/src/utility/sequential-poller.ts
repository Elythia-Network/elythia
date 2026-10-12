/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Elythia: polls one request at a time (#3463 (6c)).
 *
 * The next request is scheduled only after the previous one settled, and a
 * stop (or a restart) drops what is in flight: a response that arrives after
 * it is not handed to `tick`'s caller.
 */
export type SequentialPoller = {
	start(): void;
	stop(): void;
};

/**
 * @param tick runs one request. It receives `isCurrent`, which turns false
 *   once the poller was stopped or restarted after the request began; check
 *   it after every await before touching state. Return false to stop.
 * @param intervalMs the wait between the end of a request and the next.
 */
export function createSequentialPoller(
	tick: (isCurrent: () => boolean) => Promise<boolean>,
	intervalMs: number,
	timers: { setTimeout: typeof window.setTimeout; clearTimeout: typeof window.clearTimeout } = window,
): SequentialPoller {
	let timer: number | null = null;
	// 止めるか始め直すたびに上げ、前の世代の応答と予約を捨てる。
	let generation = 0;

	async function run(gen: number): Promise<void> {
		const isCurrent = () => gen === generation;
		let again = false;
		try {
			again = await tick(isCurrent);
		} catch {
			// tick が投げたときも読み直しは続ける (一時的な失敗は tick の側で表示する)。
			again = true;
		}
		// 前の要求が終わってから次を予約する。間隔で回すと、応答の遅い間 (メンテナンスへの
		// 出入りなど) に要求が重なり、古い応答が新しい状態を上書きする。
		if (again && isCurrent()) {
			timer = timers.setTimeout(() => { void run(gen); }, intervalMs);
		}
	}

	function stop(): void {
		generation++;
		if (timer != null) {
			timers.clearTimeout(timer);
			timer = null;
		}
	}

	return {
		start(): void {
			stop();
			void run(generation);
		},
		stop,
	};
}
