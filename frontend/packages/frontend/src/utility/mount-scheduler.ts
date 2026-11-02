/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Elythia: 古いものを読み足したときに、中身の組み立てを数フレームに分ける (#3937)。
// 一覧の配列を数件ずつ足す形は、足すたびに一覧全体 (数百件) の差分の計算と
// TransitionGroup の位置の計測が走り、分けない場合より合計で重くなった (計測で確認)。
// 配列は一度に足し、各項目の中身だけをここで順番に組み立てる。

/** Waits until the browser has had a chance to paint. */
export function nextFrame(hiddenFallbackMs = 100): Promise<void> {
	return new Promise(resolve => {
		// rAF は裏のタブでは呼ばれないので、裏にあるときだけタイマーで進める。表のタブで
		// タイマーも張ると、重いフレームの後にタイマーが rAF より先に発火し、フレームの
		// 外で組み立てて 1 フレームに 2 回分を詰め込む (レビュー M-2、計測で確認)
		if (window.document.visibilityState === 'hidden') {
			// 裏のタブのタイマーは 1 秒以上に間引かれるので、表に戻ったらすぐに進める
			const finish = () => {
				window.clearTimeout(timer);
				window.document.removeEventListener('visibilitychange', finish);
				resolve();
			};
			const timer = window.setTimeout(finish, hiddenFallbackMs);
			window.document.addEventListener('visibilitychange', finish);
		} else {
			window.requestAnimationFrame(() => resolve());
		}
	});
}

export type MountScheduler = {
	/** Queues `start` to run on a later frame. Returns a function that cancels it. */
	request(start: () => void): () => void;
	/** The number of requests still waiting. */
	readonly pending: number;
};

/**
 * Creates a scheduler that lets `perFrame` requests start on each frame, in the order
 * they were made.
 */
export function createMountScheduler(perFrame: number, waitFrame: () => Promise<void> = nextFrame): MountScheduler {
	const queue = new Set<() => void>();
	let running = false;

	async function run() {
		running = true;
		while (queue.size > 0) {
			await waitFrame();
			let started = 0;
			for (const start of queue) {
				if (started >= perFrame) break;
				queue.delete(start);
				start();
				started++;
			}
		}
		running = false;
	}

	return {
		request(start) {
			// 同じ関数を 2 回積むと Set で 1 つになるので、包んで別のものにする
			const entry = () => start();
			queue.add(entry);
			if (!running) void run();
			return () => {
				queue.delete(entry);
			};
		},
		get pending() {
			return queue.size;
		},
	};
}

/**
 * Wraps a list's `fetchOlder` so that the list can tell which items appeared because of it.
 *
 * `shouldDefer(id)` is true for items older than everything the list showed when the wrapped
 * call started, while it runs (including the render that shows the fetched items). The list
 * passes it to `MkDeferredMount`'s `defer`.
 */
export function deferOlderMounts(
	fetchOlder: () => Promise<void>,
	currentIds: () => string[],
): { fetchOlder: () => Promise<void>; shouldDefer: (id: string) => boolean } {
	let boundary: string | null = null;
	let running = false;
	return {
		async fetchOlder() {
			// 実行中にもう一度呼ばれたら (v-appear は後から呼び直す)、境に触らない。
			// paginator は 2 回目をすぐに返すので、ここで境を消すと 1 回目の応答の分が
			// 後回しにならない (レビュー M-1)
			if (running) return fetchOlder();
			running = true;
			// 新着は届いた時点で手持ちより新しい id を持つので、境より古いものだけを
			// 後回しにすれば、応答を待つ間に届いた新着まで後回しにしない (id は時系列順)
			boundary = currentIds().reduce<string | null>((min, id) => (min == null || id < min ? id : min), null);
			try {
				await fetchOlder();
			} finally {
				// 足した分の描画は、足した時点で Vue が積んだマイクロタスクで走り、この await の
				// 続きより先に終わる。なので描画の間はまだ境が残っている
				boundary = null;
				running = false;
			}
		},
		shouldDefer: (id) => boundary != null && id < boundary,
	};
}

// ノート 1 件の組み立てはスマートフォン相当で約 15ms かかるので、4 件で 1 フレーム
// あたり約 60ms に収まる。全部の一覧で 1 つを共有し、合わせてこの件数にする
export const mountScheduler = createMountScheduler(4);
