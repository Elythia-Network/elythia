/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import {
	VERSUS_BOARD_MAX_BODIES,
	VERSUS_DISCONNECT_AFTER_MS,
	compactBoard,
	formatRemaining,
	parseBoard,
	isRetryableReportError,
	opponentOf,
	outcomeFor,
	replayPlayability,
	remainingMs,
	shouldClaimDisconnected,
	sideOf,
} from '@/utility/bubble-versus-rules.js';
import type { VersusMatch } from '@/utility/bubble-versus.js';

const alice = { id: 'alice', username: 'alice' } as VersusMatch['user1'];
const bob = { id: 'bob', username: 'bob' } as VersusMatch['user2'];

function match(overrides: Partial<VersusMatch> = {}): VersusMatch {
	return {
		id: 'm1',
		gameMode: 'normal',
		status: 'playing',
		seed: 'seed',
		createdAt: '2026-09-29T00:00:00.000Z',
		startAt: null,
		endedAt: null,
		winnerId: null,
		reason: null,
		user1Id: 'alice',
		user2Id: 'bob',
		user1: alice,
		user2: bob,
		user1Ready: false,
		user2Ready: false,
		user1Result: null,
		user2Result: null,
		...overrides,
	};
}

describe('bubble game versus rules (#3231)', () => {
	test('sideOf / opponentOf', () => {
		const m = match();
		expect(sideOf(m, 'alice')).toBe(0);
		expect(sideOf(m, 'bob')).toBe(1);
		expect(sideOf(m, 'carol')).toBe(-1);
		expect(sideOf(m, null)).toBe(-1);
		expect(opponentOf(m, 'alice')).toBe(bob);
		expect(opponentOf(m, 'bob')).toBe(alice);
		expect(opponentOf(m, 'carol')).toBeNull();
	});

	test('remainingMs counts from the local start and never goes negative', () => {
		expect(remainingMs(1_000, 300_000, 1_000)).toBe(300_000);
		expect(remainingMs(1_000, 300_000, 61_000)).toBe(240_000);
		expect(remainingMs(1_000, 300_000, 301_000)).toBe(0);
		expect(remainingMs(1_000, 300_000, 999_999)).toBe(0);
	});

	test('formatRemaining rounds up so 0:00 means time is up', () => {
		expect(formatRemaining(300_000)).toBe('5:00');
		expect(formatRemaining(61_000)).toBe('1:01');
		expect(formatRemaining(1)).toBe('0:01');
		expect(formatRemaining(0)).toBe('0:00');
		expect(formatRemaining(-5)).toBe('0:00');
	});

	test('shouldClaimDisconnected after silence', () => {
		const base = { localStartedAt: 0, timeLimitMs: 300_000, reported: false };
		expect(shouldClaimDisconnected({ ...base, now: 100_000, lastHeardFromOpponent: 100_000 - VERSUS_DISCONNECT_AFTER_MS + 1 })).toBe(false);
		expect(shouldClaimDisconnected({ ...base, now: 100_000, lastHeardFromOpponent: 100_000 - VERSUS_DISCONNECT_AFTER_MS })).toBe(true);
	});

	test('shouldClaimDisconnected after the time limit only when this side reported', () => {
		const after = 300_000 + VERSUS_DISCONNECT_AFTER_MS;
		const base = { localStartedAt: 0, timeLimitMs: 300_000, lastHeardFromOpponent: after };
		expect(shouldClaimDisconnected({ ...base, now: after, reported: false })).toBe(false);
		expect(shouldClaimDisconnected({ ...base, now: after - 1, reported: true })).toBe(false);
		expect(shouldClaimDisconnected({ ...base, now: after, reported: true })).toBe(true);
	});

	test('isRetryableReportError retries transient failures only', () => {
		// 時計のずれ・通信の失敗・サーバーの一時的な失敗はやり直す。
		for (const code of ['NOT_YET', 'INTERNAL_ERROR', undefined, null, 'RATE_LIMIT_EXCEEDED']) {
			expect(isRetryableReportError(code)).toBe(true);
		}
		// 同じ報告にはまた同じ答えが返るものはやり直さない。
		for (const code of ['INVALID_STATE', 'INVALID_REPORT', 'INVALID_PARAM', 'NO_SUCH_MATCH']) {
			expect(isRetryableReportError(code)).toBe(false);
		}
	});

	test('compactBoard rounds and caps the number of bodies', () => {
		expect(compactBoard([{ x: 10.4, y: 20.6, r: 7.5, level: 3 }, { x: 1, y: 2, r: 3, level: 0 }])).toEqual([[10, 21, 8, 3], [1, 2, 3, 0]]);
		const many = Array.from({ length: VERSUS_BOARD_MAX_BODIES + 10 }, () => ({ x: 1, y: 1, r: 1, level: 1 }));
		expect(compactBoard(many)).toHaveLength(VERSUS_BOARD_MAX_BODIES);
		// 上限いっぱいでもサーバーの上限 (8KB) に収まる。
		expect(JSON.stringify({ score: 999999, pending: 200, danger: true, gameOver: false, board: compactBoard(many.map(() => ({ x: 450, y: 600, r: 120, level: 11 }))) }).length).toBeLessThan(8 * 1024);
	});

	test('parseBoard keeps only well-formed entries from the opponent', () => {
		expect(parseBoard(null)).toEqual([]);
		expect(parseBoard('x')).toEqual([]);
		expect(parseBoard([[1, 2, 3, 4], [1, 2, 3], [1, 2, 'a', 4], [1, 2, 0, 4], [1, 2, 9999, 4], [1, 2, Infinity, 4], 'x', [5, 6, 7, 0]])).toEqual([
			{ x: 1, y: 2, r: 3, level: 4 },
			{ x: 5, y: 6, r: 7, level: 0 },
		]);
		expect(parseBoard(Array.from({ length: VERSUS_BOARD_MAX_BODIES + 5 }, () => [1, 1, 1, 1]))).toHaveLength(VERSUS_BOARD_MAX_BODIES);
	});

	test('outcomeFor', () => {
		expect(outcomeFor(match(), 'alice')).toBeNull();
		expect(outcomeFor(match({ status: 'ended', winnerId: 'alice' }), 'alice')).toBe('win');
		expect(outcomeFor(match({ status: 'ended', winnerId: 'alice' }), 'bob')).toBe('lose');
		expect(outcomeFor(match({ status: 'ended', winnerId: null }), 'bob')).toBe('draw');
	});
});

describe('bubble game versus replay (mk-go #3232)', () => {
	test('記録と版がそろった盤面だけ再生する', () => {
		expect(replayPlayability({ logs: [[1, 0, 5]], result: { gameVersion: 4 }, engineVersion: 4 })).toBe('ok');
	});

	test('記録が無い盤面 (切断した側など) は再生しない', () => {
		expect(replayPlayability({ logs: null, result: { gameVersion: 4 }, engineVersion: 4 })).toBe('noLogs');
		expect(replayPlayability({ logs: undefined, result: null, engineVersion: 4 })).toBe('noLogs');
		expect(replayPlayability({ logs: [[1, 0, 5]], result: null, engineVersion: 4 })).toBe('noLogs');
		// ロビーでの投了は盤面が動かない。版が無くても、記録が空なら「記録が無い」。
		expect(replayPlayability({ logs: [], result: { gameVersion: 4 }, engineVersion: 4 })).toBe('noLogs');
		expect(replayPlayability({ logs: [], result: { gameVersion: null }, engineVersion: 4 })).toBe('noLogs');
	});

	test('版が違う・版が分からない盤面は再生しない', () => {
		expect(replayPlayability({ logs: [[1, 0, 5]], result: { gameVersion: 3 }, engineVersion: 4 })).toBe('versionMismatch');
		expect(replayPlayability({ logs: [[1, 0, 5]], result: { gameVersion: 5 }, engineVersion: 4 })).toBe('versionMismatch');
		expect(replayPlayability({ logs: [[1, 0, 5]], result: { gameVersion: null }, engineVersion: 4 })).toBe('versionMismatch');
	});
});
