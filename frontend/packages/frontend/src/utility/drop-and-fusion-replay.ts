/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// mk-go (#3232): 記録からゲームを進める。対戦のリプレイの盤面
// (drop-and-fusion.versus.replay-board.vue) が使う。画面から切り離してあるのは、
// 「同じシードと記録から同じ結末になる」をテストで固定するため (操作を当てる
// フレームが 1 つずれるだけで別の結末になる)。

import type { DropAndFusionGame, Log } from 'misskey-bubble-game';

export type ReplayCursor = {
	/**
	 * Advances the game to `frame` (capped at the end frame). Returns false once
	 * the replay has ended.
	 */
	advanceTo(frame: number): boolean;
	readonly done: boolean;
};

/**
 * Creates a cursor that replays `logs` on a started `game` up to `endFrame`.
 * Operations recorded at a frame are applied before that frame is ticked, the
 * same way the in-game replay does.
 */
export function createReplayCursor(game: DropAndFusionGame, logs: Log[], endFrame: number): ReplayCursor {
	let next = 0;
	let done = false;
	return {
		advanceTo(frame: number): boolean {
			const target = Math.min(frame, endFrame);
			while (!done && game.frame < target) {
				// 同じフレームの操作は全部当てる (対局の画面のリプレイと同じ)。
				while (next < logs.length && logs[next].frame === game.frame) {
					game.applyLog(logs[next++]);
				}
				if (!game.tick()) done = true;
			}
			if (game.frame >= endFrame) done = true;
			return !done;
		},
		get done() {
			return done;
		},
	};
}
