/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// mk-go (#3231): バブルゲームの 1:1 対戦 (#3228)。API とチャンネルは mk-go 独自なので
// misskey-js の型に無い。型はここで持ち、呼び出しもここに寄せる。

import type * as Misskey from 'misskey-js';
import type { GameMode } from 'misskey-bubble-game';
import { misskeyApi } from '@/utility/misskey-api.js';
import { useStream } from '@/stream.js';

export type VersusStatus = 'invited' | 'accepted' | 'playing' | 'ended';
export type VersusReason = 'gameOver' | 'surrender' | 'timeUp' | 'disconnected' | 'invalidReport';

/**
 * The reason of one player's report. `opponentEnded` is sent after the
 * opponent's report ended the match, only to keep the board for the replay
 * (mk-go #3232). It never decides the outcome.
 */
export type VersusReportReason = 'gameOver' | 'surrender' | 'timeUp' | 'opponentEnded';

export type VersusResultSummary = {
	score: number;
	frame: number;
	reason: VersusReason | 'opponentEnded';
};

/**
 * A versus match as returned by the `bubble-game/versus/*` endpoints.
 */
export type VersusMatch = {
	id: string;
	gameMode: GameMode;
	status: VersusStatus;
	seed: string | null;
	createdAt: string;
	startAt: string | null;
	endedAt: string | null;
	winnerId: string | null;
	reason: VersusReason | null;
	user1Id: string;
	user2Id: string;
	user1: Misskey.entities.UserLite | null;
	user2: Misskey.entities.UserLite | null;
	user1Ready: boolean;
	user2Ready: boolean;
	user1Result: VersusResultSummary | null;
	user2Result: VersusResultSummary | null;
};

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

export type VersusReport = {
	score: number;
	frame: number;
	reason: VersusReportReason;
	logs: number[][];
	/** The engine version the logs were made with (mk-go #3232). */
	gameVersion?: number;
};

/**
 * One player's side of a stored match record (mk-go #3232).
 */
export type VersusRecordResult = {
	score: number;
	frame: number;
	reason: VersusReason | 'opponentEnded';
	/** The engine version the logs were made with. null when the client did not send it. */
	gameVersion: number | null;
};

/**
 * A finished match as returned by `bubble-game/versus/history` and `record`.
 * `seed` and the logs are only present on `record`.
 */
export type VersusRecord = {
	id: string;
	gameMode: GameMode;
	startedAt: string;
	endedAt: string | null;
	winnerId: string | null;
	reason: VersusReason | null;
	isPublic: boolean;
	user1Id: string;
	user2Id: string;
	user1: Misskey.entities.UserLite | null;
	user2: Misskey.entities.UserLite | null;
	user1Public: boolean;
	user2Public: boolean;
	user1Result: VersusRecordResult | null;
	user2Result: VersusRecordResult | null;
	seed?: string;
	user1Logs?: number[][] | null;
	user2Logs?: number[][] | null;
};

export function versusApi<T = unknown>(endpoint: string, params: Record<string, unknown> = {}): Promise<T> {
	// mk-go 独自の endpoint は misskey-js の型に無いので、ここでだけ型を外す。
	return misskeyApi(`bubble-game/versus/${endpoint}` as never, params as never) as Promise<T>;
}

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
