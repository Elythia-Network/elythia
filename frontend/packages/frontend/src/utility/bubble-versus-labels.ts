/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// mk-go (#3232): 対戦の結果の文言。対局の画面と対戦の履歴で同じものを出す。

import { i18n } from '@/i18n.js';
import type { VersusReason } from '@/utility/bubble-versus.js';

/** The label of how a match was decided. */
export function versusReasonLabel(reason: VersusReason | null | undefined): string {
	switch (reason) {
		case 'gameOver': return i18n.ts._mkgoBubbleGame._versus.reasonGameOver;
		case 'surrender': return i18n.ts._mkgoBubbleGame._versus.reasonSurrender;
		case 'timeUp': return i18n.ts._mkgoBubbleGame._versus.reasonTimeUp;
		case 'disconnected': return i18n.ts._mkgoBubbleGame._versus.reasonDisconnected;
		case 'invalidReport': return i18n.ts._mkgoBubbleGame._versus.reasonInvalidReport;
		default: return '';
	}
}

/** The label of an outcome from one player's point of view. */
export function versusOutcomeLabel(outcome: 'win' | 'lose' | 'draw' | null): string {
	switch (outcome) {
		case 'win': return i18n.ts._mkgoBubbleGame._versus.win;
		case 'lose': return i18n.ts._mkgoBubbleGame._versus.lose;
		case 'draw': return i18n.ts._mkgoBubbleGame._versus.draw;
		default: return '';
	}
}
