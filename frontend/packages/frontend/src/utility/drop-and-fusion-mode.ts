/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { parseGameMode } from 'misskey-bubble-game';

/**
 * Returns the unit of the score of the bubble game mode (mk-go, #3216).
 *
 * 単位は形 (yen / sweets) で決まり、物理では変わらない。`yen-bouncy` のような
 * 組み合わせも同じ単位にする。
 */
export function dropAndFusionScoreUnit(gameMode: string): string {
	switch (parseGameMode(gameMode)?.base) {
		case 'yen': return '円';
		case 'sweets': return 'kcal';
		default: return 'pt';
	}
}

/**
 * Returns the label of the bubble game mode shown to users, such as
 * `SQUARE × BOUNCY` (mk-go, #3216).
 */
export function dropAndFusionModeLabel(gameMode: string): string {
	const parsed = parseGameMode(gameMode);
	if (parsed == null) return gameMode.toUpperCase();
	const base = parsed.base.toUpperCase();
	return parsed.physics === 'default' ? base : `${base} × ${parsed.physics.toUpperCase()}`;
}
