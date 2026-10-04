/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// mk-go (#3231): バブルゲームの対戦の、画面から切り離せる判断。API やストリームを
// import しないので単体テストから読める。

import type * as Misskey from 'misskey-js';
import type { VersusMatch } from '@/utility/bubble-versus.js';

// 報告を送れるかの目安。サーバーは制限時間の 5 秒前から時間切れの報告を受ける。
export const VERSUS_TIME_UP_SKEW_MS = 5_000;
// 相手が黙っていたら切断とみなせるまでの時間。サーバーと同じ値にする。
export const VERSUS_DISCONNECT_AFTER_MS = 30_000;
// 盤面の要約を送る間隔。相手の画面に盤面を小さく出すので 1 秒ごとにする
// (切断の判定 30 秒より十分短い)。
export const VERSUS_STATE_INTERVAL_MS = 1_000;
// 送る玉の数の上限。盤面に乗る数はこれより十分少ないが、壊れた状態で 8KB
// (サーバーの上限) を超えて中継されなくなるのを防ぐ。
export const VERSUS_BOARD_MAX_BODIES = 150;

/**
 * Compacts bodies into the board summary sent to the opponent.
 */
export function compactBoard(bodies: { x: number; y: number; r: number; level: number }[]): number[][] {
	return bodies.slice(0, VERSUS_BOARD_MAX_BODIES).map(b => [Math.round(b.x), Math.round(b.y), Math.round(b.r), b.level]);
}

/**
 * Parses a board summary from the opponent, dropping malformed entries.
 * 相手から届く値なので形を確かめてから描く。
 */
export function parseBoard(board: unknown): { x: number; y: number; r: number; level: number }[] {
	if (!Array.isArray(board)) return [];
	const out: { x: number; y: number; r: number; level: number }[] = [];
	for (const e of board.slice(0, VERSUS_BOARD_MAX_BODIES)) {
		if (!Array.isArray(e) || e.length !== 4 || !e.every(v => typeof v === 'number' && Number.isFinite(v))) continue;
		const [x, y, r, level] = e as number[];
		if (r <= 0 || r > 500) continue;
		out.push({ x, y, r, level });
	}
	return out;
}

// 報告をやり直すまでの待ち時間。時間切れの報告は、端末の時計が進んでいると
// サーバーがまだ受けない (NOT_YET) ので、その分も含めて数十秒は粘る。
export const REPORT_RETRY_DELAYS_MS = [1_000, 2_000, 3_000, 5_000, 8_000, 13_000];

/**
 * Whether a failed report is worth sending again. Errors that the server will
 * give again for the same report (the match is over, the report is malformed,
 * this user is not a player) are not.
 */
export function isRetryableReportError(code: string | null | undefined): boolean {
	switch (code) {
		case 'INVALID_STATE':
		case 'INVALID_REPORT':
		case 'INVALID_PARAM':
		case 'NO_SUCH_MATCH':
			return false;
		default:
			return true;
	}
}

/** Returns which side (0 or 1) userId plays in the match, or -1. */
export function sideOf(match: Pick<VersusMatch, 'user1Id' | 'user2Id'>, userId: string | null | undefined): 0 | 1 | -1 {
	if (match.user1Id === userId) return 0;
	if (match.user2Id === userId) return 1;
	return -1;
}

/** Returns the opponent of userId in the match. */
export function opponentOf(match: VersusMatch, userId: string | null | undefined): Misskey.entities.UserLite | null {
	const side = sideOf(match, userId);
	if (side === 0) return match.user2;
	if (side === 1) return match.user1;
	return null;
}

/**
 * Milliseconds left of the time limit, counted from when the game started on
 * this device (never negative).
 *
 * 端末の時計はサーバーとずれているので、サーバーの startAt ではなく、この端末で
 * 始めた時刻から数える。
 */
export function remainingMs(localStartedAt: number, timeLimitMs: number, now: number): number {
	return Math.max(0, localStartedAt + timeLimitMs - now);
}

/** Formats milliseconds as m:ss (rounded up, so 0:00 means the time is up). */
export function formatRemaining(ms: number): string {
	const total = Math.ceil(Math.max(0, ms) / 1000);
	const m = Math.floor(total / 60);
	const s = total % 60;
	return `${m}:${s.toString().padStart(2, '0')}`;
}

/**
 * Whether it is worth asking the server to rule the opponent disconnected.
 *
 * サーバーが最終的に判定するので、ここは無駄な申告を減らすための目安。
 * 相手から 30 秒何も届いていないか、自分は報告を済ませて制限時間から 30 秒過ぎたか。
 */
export function shouldClaimDisconnected(opts: {
	now: number;
	lastHeardFromOpponent: number;
	localStartedAt: number;
	timeLimitMs: number;
	reported: boolean;
}): boolean {
	if (opts.now - opts.lastHeardFromOpponent >= VERSUS_DISCONNECT_AFTER_MS) return true;
	return opts.reported && opts.now >= opts.localStartedAt + opts.timeLimitMs + VERSUS_DISCONNECT_AFTER_MS;
}

/**
 * The outcome from the viewer's point of view.
 */
export function outcomeFor(match: Pick<VersusMatch, 'status' | 'winnerId'>, userId: string | null | undefined): 'win' | 'lose' | 'draw' | null {
	if (match.status !== 'ended') return null;
	if (match.winnerId == null) return 'draw';
	return match.winnerId === userId ? 'win' : 'lose';
}

/**
 * Whether one board of a stored match can be replayed (mk-go #3232).
 *
 * 記録が無い盤面 (切断した側など) は再生しない。版が違う盤面も再生しない — 同じ
 * 記録でも、ルールや物理が変わった版のエンジンでは別の結末になる。版を送らない
 * 古いクライアントの記録は、どの版で遊ばれたか分からないので再生しない。
 */
export function replayPlayability(opts: {
	logs: number[][] | null | undefined;
	result: { gameVersion: number | null } | null | undefined;
	engineVersion: number;
}): 'ok' | 'noLogs' | 'versionMismatch' {
	// 空の記録 (ロビーでの投了) は盤面が動かないので、記録が無いのと同じに扱う。
	if (opts.logs == null || opts.logs.length === 0 || opts.result == null) return 'noLogs';
	if (opts.result.gameVersion == null || opts.result.gameVersion !== opts.engineVersion) return 'versionMismatch';
	return 'ok';
}
