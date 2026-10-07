/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// mk-go (#3231): バブルゲームの 1:1 対戦 (#3228)。API の型は elythia-js が持つ
// (#3431)。チャンネルは misskey-js の型に無いので、ここで持つ。

import type * as Elythia from 'elythia-js';
import type { GameMode } from 'misskey-bubble-game';
import { useStream } from '@/stream.js';

export type VersusStatus = Elythia.BubbleVersusStatus;
export type VersusReason = Elythia.BubbleVersusReason;
export type VersusReportReason = Elythia.BubbleVersusReportReason;
export type VersusResultSummary = Elythia.BubbleVersusResultSummary;
export type VersusMatch = Elythia.BubbleVersusMatch;

/**
 * The board summary each player relays to the opponent every few seconds.
 */
export type VersusBoardState = {
	score: number;
	pending: number;
	danger: boolean;
	gameOver: boolean;
	/**
	 * The bodies on the board as [x, y, radius, level] (level 0 is a stone), in
	 * game coordinates, rounded to integers. Absent from older clients.
	 */
	board?: number[][];
};

export type VersusStarted = {
	seed: string;
	gameMode: GameMode;
	startAt: number;
	countdownMs: number;
	timeLimitMs: number;
};

/** What the game reports at the end; the page adds the match id. */
export type VersusReport = Omit<Elythia.Endpoints['bubble-game/versus/report']['req'], 'matchId'>;

export type VersusRecordResult = Elythia.BubbleVersusRecordResult;
export type VersusRecord = Elythia.BubbleVersusRecord;

type Listener = (payload: any) => void;

/**
 * The subset of a stream connection the versus pages use.
 */
export type VersusConnection = {
	on(event: string, listener: Listener): void;
	off(event: string, listener: Listener): void;
	send(type: string, body: unknown): void;
	dispose(): void;
};

/** Connects to the per-user invitation channel. */
export function connectVersusInvitations(): VersusConnection {
	// misskey-js の Channels に無いチャンネルなので型を当て直す。
	return useStream().useChannel('bubbleVersus' as never) as unknown as VersusConnection;
}

/** Connects to the channel of one match (participants only). */
export function connectVersusMatch(matchId: string): VersusConnection {
	return useStream().useChannel('bubbleVersusMatch' as never, { matchId } as never) as unknown as VersusConnection;
}
