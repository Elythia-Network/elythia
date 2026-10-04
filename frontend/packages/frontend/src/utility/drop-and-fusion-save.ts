/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { miLocalStorage } from '@/local-storage.js';
import { $i } from '@/i.js';

/**
 * A suspended bubble game (mk-go, #3192).
 *
 * **盤面は持たない。** ゲームは決定的に動くので、シードと操作の記録だけで同じ
 * 盤面に戻せる (リプレイと同じ仕組み)。`l` は `DropAndFusionGame.serializeLogs` の形。
 */
export type DropAndFusionSave = {
	/** GAME_VERSION at the time of saving. */
	v: number;
	/** Game mode. */
	m: string;
	/** Seed. */
	s: string;
	/** Serialized operation logs. */
	l: number[][];
};

// **アカウントごとに分ける。** 同じブラウザで別のアカウントに切り替えたとき、
// 他人の途中のゲームを続きから遊べてしまわないように。
function key(gameMode: string) {
	return `mkgo:dropAndFusion:${$i?.id ?? 'guest'}:${gameMode}` as const;
}

/**
 * How old a save (its seed) may be to be continued (mk-go, #3192).
 *
 * backend は 7 日より古いシードのスコアを登録しない (`internal/api/bubblegame` の
 * seedMaxAge)。**1 日の余裕を残す** — 期限ぎりぎりに再開すると、終える前に期限が
 * 来て登録できない。
 */
export const SAVE_MAX_AGE_MS = 6 * 24 * 60 * 60 * 1000;

// 早送りする長さの上限 (60fps で 6 時間ぶん)。localStorage は書き換えられるので、
// 巨大なフレーム差を入れた保存で早送りが終わらなくなるのを防ぐ。
const MAX_FRAMES = 60 * 60 * 60 * 6;

function isSave(v: unknown): v is DropAndFusionSave {
	if (v == null || typeof v !== 'object') return false;
	const o = v as Record<string, unknown>;
	if (typeof o.v !== 'number' || typeof o.m !== 'string' || typeof o.s !== 'string' || !Array.isArray(o.l)) return false;
	let frames = 0;
	for (const x of o.l) {
		if (!Array.isArray(x) || !x.every(y => Number.isInteger(y))) return false;
		// フレーム差が小数だと記録のフレームに一致せず、操作が黙って飛ばされる。
		if (x.length < 2 || x[0] < 0) return false;
		// 落とす操作 (種別 0) は x 座標を持つ。欠けていると NaN の位置に玉ができる。
		if (x[1] === 0 && x.length < 3) return false;
		frames += x[0];
	}
	return frames <= MAX_FRAMES;
}

/**
 * Whether a save is too old to be continued (its score could no longer be
 * registered).
 */
export function isDropAndFusionSaveExpired(save: DropAndFusionSave, now = Date.now()): boolean {
	const seededAt = Number(save.s);
	return !Number.isFinite(seededAt) || now - seededAt > SAVE_MAX_AGE_MS;
}

/**
 * Returns the saved game for the mode, or `null` when there is none (or it is
 * unreadable).
 *
 * **localStorage は使えないことがある** (プライベートブラウズ、容量超過、サイト
 * データの削除)。どれも「保存が無い」として扱い、ゲーム自体は止めない。
 */
export function loadDropAndFusionSave(gameMode: string): DropAndFusionSave | null {
	try {
		const raw = miLocalStorage.getItem(key(gameMode));
		if (raw == null) return null;
		const parsed: unknown = JSON.parse(raw);
		if (!isSave(parsed) || parsed.m !== gameMode) return null;
		return parsed;
	} catch {
		return null;
	}
}

export function writeDropAndFusionSave(save: DropAndFusionSave): void {
	try {
		miLocalStorage.setItem(key(save.m), JSON.stringify(save));
	} catch {
		// 保存できなくてもゲームは続ける。
	}
}

export function clearDropAndFusionSave(gameMode: string): void {
	try {
		miLocalStorage.removeItem(key(gameMode));
	} catch {
		// nop
	}
}
