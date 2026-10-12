/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/** The kind of the seasonal screen effect. */
export type SeasonalScreenEffect = 'snow' | 'sakura';

/**
 * Returns the seasonal screen effect for the month, or null when the month has none.
 *
 * @param month The month, 1 to 12.
 * @param hemisphere The hemisphere the user chose (`prefer.s.hemisphere`).
 */
export function seasonalScreenEffectOf(month: number, hemisphere: 'N' | 'S'): SeasonalScreenEffect | null {
	if (hemisphere === 'S') {
		// ▼南半球
		if (month === 7 || month === 8) return 'snow';
		return null;
	}

	// ▼北半球
	if (month === 12 || month === 1) return 'snow';
	if (month === 3 || month === 4) return 'sakura';
	return null;
}
