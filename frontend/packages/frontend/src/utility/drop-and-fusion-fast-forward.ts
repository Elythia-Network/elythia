/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type { DropAndFusionGame } from 'misskey-bubble-game';

type Logs = ReturnType<typeof DropAndFusionGame.deserializeLogs>;

export type FastForwardResult = 'done' | 'gameOver' | 'cancelled';

/**
 * Replays recorded operations on a freshly started game without rendering, to
 * continue a suspended game (mk-go, #3192).
 *
 * 記録のフレームに来たら操作を当ててから tick する (実際のプレイと同じ順序)。最後の
 * 操作のフレームの tick まで進めて返す — その後は通常の tick に引き継ぐ。
 *
 * **少しずつ進める。** 長いゲームは数万フレームになるので、1 回で回すと画面が
 * 固まる。`budgetMs` ぶん進めたら `yieldToBrowser` で制御を返す。
 *
 * **中止を毎回確かめる。** 早送り中に画面を離れると、ゲームは dispose されて壁も床も
 * 消えている。そのまま回すと止まらない (ゲームオーバーにならない) ループが残る。
 */
export async function fastForwardGame(game: DropAndFusionGame, logs: Logs, opts: {
	isCancelled: () => boolean;
	onProgress?: (progress: number) => void;
	yieldToBrowser?: () => Promise<void>;
	now?: () => number;
	budgetMs?: number;
}): Promise<FastForwardResult> {
	const lastFrame = logs.length > 0 ? logs[logs.length - 1].frame : 0;
	const now = opts.now ?? (() => window.performance.now());
	const yieldToBrowser = opts.yieldToBrowser ?? (() => new Promise<void>(resolve => window.requestAnimationFrame(() => resolve())));
	const budgetMs = opts.budgetMs ?? 12;
	let next = 0;

	while (game.frame <= lastFrame) {
		if (opts.isCancelled()) return 'cancelled';
		const deadline = now() + budgetMs;
		while (game.frame <= lastFrame && now() < deadline) {
			// 同じフレームに操作が 2 つ (保持してすぐ落とす) あり得るので、全部当てる。
			while (next < logs.length && logs[next].frame === game.frame) {
				// 種類ごとの分岐はエンジン側 (applyLog) に任せる。ここで書くと、種類が
				// 増えたとき (対戦の garbage、#3229) に surrender として扱ってしまう。
				game.applyLog(logs[next++]);
			}
			if (!game.tick()) return 'gameOver';
		}
		opts.onProgress?.(Math.min(1, game.frame / Math.max(1, lastFrame)));
		await yieldToBrowser();
	}
	return opts.isCancelled() ? 'cancelled' : 'done';
}
