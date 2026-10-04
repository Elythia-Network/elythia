/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { beforeEach, describe, expect, test, vi } from 'vitest';
import * as Matter from 'matter-js';
import { DropAndFusionGame, gameModeOf, parseGameMode, VERSUS_RULES } from 'misskey-bubble-game';
import type { BaseGameMode, GamePhysics } from 'misskey-bubble-game';

vi.mock('@/i.js', () => ({ $i: { id: 'user1' } }));
import { clearDropAndFusionSave, isDropAndFusionSaveExpired, loadDropAndFusionSave, SAVE_MAX_AGE_MS, writeDropAndFusionSave } from '@/utility/drop-and-fusion-save.js';
import { fastForwardGame } from '@/utility/drop-and-fusion-fast-forward.js';
import { createReplayCursor } from '@/utility/drop-and-fusion-replay.js';
import { dropAndFusionModeLabel, dropAndFusionScoreUnit } from '@/utility/drop-and-fusion-mode.js';

type Mode = ConstructorParameters<typeof DropAndFusionGame>[0]['gameMode'];

function newGame(mode: Mode, seed: string) {
	// 描画設定は本番では必ず渡される。渡さないと matter-js が render の既定値を
	// 埋められずに落ちる。
	return new DropAndFusionGame({ seed, gameMode: mode, getMonoRenderOptions: () => ({}) });
}

// 決定的な乱数 (mulberry32)。ボットの落とす位置に使う。
function botRng(seed: number) {
	let a = seed;
	return () => {
		a |= 0; a = a + 0x6D2B79F5 | 0;
		let t = Math.imul(a ^ a >>> 15, 1 | a);
		t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
		return ((t ^ t >>> 14) >>> 0) / 4294967296;
	};
}

/**
 * Plays a game with a bot that drops at random positions every 35 frames until
 * the game is over.
 */
function playUntilOver(mode: Mode, seed: string, bot: number) {
	const g = newGame(mode, seed);
	const rng = botRng(bot);
	let score = 0;
	let over = false;
	let warningSince: number | null = null;
	let recoveries = 0;
	let warnedForAtOver = 0;
	g.on('changeScore', v => { score = v; });
	g.on('overflowWarning', v => {
		if (v) {
			warningSince = g.frame;
		} else if (!over && warningSince != null) {
			// ゲームオーバーの後にも false が来るので、over を立てる前の分だけ数える。
			recoveries++;
			warningSince = null;
		}
	});
	g.on('gameOver', () => {
		warnedForAtOver = warningSince == null ? 0 : g.frame - warningSince;
		over = true;
	});
	g.start();
	while (!over && g.frame < 60 * 60 * 10) {
		if (g.frame % 35 === 0) g.drop(30 + rng() * 390);
		if (!g.tick()) break;
	}
	return { over, frame: g.frame, score, logs: g.getLogs(), recoveries, warnedForAtOver, grace: g.msToFrame(g.OVERFLOW_GRACE_MS) };
}

/** Re-runs recorded operations the way replay / resume do. */
function replay(mode: Mode, seed: string, serialized: number[][]) {
	const logs = DropAndFusionGame.deserializeLogs(serialized);
	const g = newGame(mode, seed);
	let score = 0;
	let overAt: number | null = null;
	g.on('changeScore', v => { score = v; });
	g.on('gameOver', () => { overAt = g.frame; });
	g.start();
	let next = 0;
	while (g.frame < 60 * 60 * 10) {
		while (next < logs.length && logs[next].frame === g.frame) {
			const log = logs[next++];
			if (log.operation === 'drop') g.drop(log.x);
			else if (log.operation === 'hold') g.hold();
			else g.surrender();
		}
		if (!g.tick()) break;
	}
	return { overAt, score };
}

/**
 * mk-go: はみ出しの判定に 2 秒の猶予を持たせる (#3193)。
 *
 * **即時判定に戻すと、警告が出た時点で終わる。** その場合「警告が解けて続いた」回数が
 * 0 になり、終わったときの警告の長さも 0 になるので、どちらのアサーションでも落ちる。
 */
describe('bubble game overflow grace (#3193)', () => {
	test('版は 4', () => {
		expect(DropAndFusionGame.VERSION).toBe(4);
		expect(newGame('normal', 's').GAME_VERSION).toBe(4);
	});

	// 終わったときに警告が 2 秒以上続いていること。**即時判定に戻すと 0 で終わる。**
	// (警告は「どれかの玉がはみ出している間」なので、玉が入れ替わると 2 秒より長くなる)
	//
	// ゲームを終わるまで回すので、既定の 5 秒では CI のランナーで上限を超える (#3224
	// と同じ)。
	test.each(['normal', 'square'] as const)('%s: 2 秒とどまったら終わる', (mode) => {
		const r = playUntilOver(mode, 'seed-a', 1);
		expect(r.over).toBe(true);
		expect(r.warnedForAtOver).toBeGreaterThanOrEqual(r.grace);
	}, 30000);

	// 一瞬はみ出しても終わらない。ゲームによっては一度も解けないまま終わるので、
	// 解ける場面があるシードで見る。
	test('一瞬はみ出しても、出れば終わらない', () => {
		const r = playUntilOver('normal', 'seed-a', 1);
		expect(r.recoveries).toBeGreaterThan(0);
	}, 30000);

	// リプレイと途中保存 (#3192) の早送りは、同じシードと操作の記録から同じ結末に
	// なることが前提。判定を実時間で数えるとここが崩れる。
	//
	// ゲームを終わるまで回してからリプレイし直すので、既定の 5 秒では CI の
	// ランナーで上限を超える (手元の 8 コアでも bouncy は 3.5 秒かかる。#3224)。
	test.each(['normal', 'square', 'bouncy', 'space'] as const)('%s: 同じシードと記録から同じ結末になる', (mode) => {
		const r = playUntilOver(mode, 'seed-b', 2);
		const serialized = DropAndFusionGame.serializeLogs(r.logs);
		const again = replay(mode, 'seed-b', serialized);
		expect(again.overAt).toBe(r.frame);
		expect(again.score).toBe(r.score);
	}, 30000);
});

/**
 * mk-go: 途中保存 (#3192)。盤面ではなくシードと操作の記録だけを持つ。
 */
describe('bubble game save (#3192)', () => {
	beforeEach(() => {
		window.localStorage.clear();
	});

	const save = { v: 4, m: 'normal', s: 'seed', l: [[10, 0, 200], [40, 1]] };

	test('書いたものを読める / 消せる', () => {
		writeDropAndFusionSave(save);
		expect(loadDropAndFusionSave('normal')).toEqual(save);
		clearDropAndFusionSave('normal');
		expect(loadDropAndFusionSave('normal')).toBeNull();
	});

	test('モードごとに別々に持つ', () => {
		writeDropAndFusionSave(save);
		writeDropAndFusionSave({ ...save, m: 'square', s: 'other' });
		expect(loadDropAndFusionSave('normal')?.s).toBe('seed');
		expect(loadDropAndFusionSave('square')?.s).toBe('other');
		expect(loadDropAndFusionSave('yen')).toBeNull();
	});

	test('アカウントごとに別のキーに書く', () => {
		writeDropAndFusionSave(save);
		expect(window.localStorage.getItem('mkgo:dropAndFusion:user1:normal')).not.toBeNull();
	});

	test('壊れた保存は無いものとして扱う', () => {
		window.localStorage.setItem('mkgo:dropAndFusion:user1:normal', '{not json');
		expect(loadDropAndFusionSave('normal')).toBeNull();
		window.localStorage.setItem('mkgo:dropAndFusion:user1:normal', JSON.stringify({ ...save, l: 'x' }));
		expect(loadDropAndFusionSave('normal')).toBeNull();
		// 別のモードの保存がそのキーに入っていても使わない。
		window.localStorage.setItem('mkgo:dropAndFusion:user1:normal', JSON.stringify({ ...save, m: 'yen' }));
		expect(loadDropAndFusionSave('normal')).toBeNull();
	});

	test('保存した記録から早送りすると中断前と同じ局面になる', () => {
		// 途中まで遊んだゲームを「保存した記録」から戻し、同じフレームまで進めた
		// ときのスコアと次の玉が一致するかを見る。
		function track(g: DropAndFusionGame) {
			const state = { score: 0, stock: [] as string[] };
			g.on('changeScore', v => { state.score = v; });
			g.on('changeStock', v => { state.stock = v.map(x => x.id); });
			return state;
		}

		const g = newGame('normal', 'resume-seed');
		const original = track(g);
		const rng = botRng(3);
		g.start();
		while (g.frame < 1500) {
			if (g.frame % 35 === 0) g.drop(30 + rng() * 390);
			g.tick();
		}
		const serialized = DropAndFusionGame.serializeLogs(g.getLogs());

		const r = newGame('normal', 'resume-seed');
		const resumed = track(r);
		r.start();
		const logs = DropAndFusionGame.deserializeLogs(serialized);
		let next = 0;
		while (r.frame < g.frame) {
			while (next < logs.length && logs[next].frame === r.frame) {
				const log = logs[next++];
				if (log.operation === 'drop') r.drop(log.x);
			}
			r.tick();
		}
		expect(original.score).toBeGreaterThan(0);
		expect(resumed.score).toBe(original.score);
		expect(resumed.stock).toEqual(original.stock);
	});
});

/**
 * mk-go: 箱の外へ抜けた玉 (#3193)。
 *
 * 本家は判定領域に触れた瞬間に終わるので、箱の外へ出た玉が残ることは無かった。猶予を
 * 持たせたので、壁を抜けた玉があれば終わらせる (起きないはずの備え)。
 */
describe('bubble game lost mono (#3193)', () => {
	// 壁を抜けて失われた玉が出たら終わらせる (起きないはずの備え)。壁の内側の面で
	// 判定すると、一瞬めり込んだだけで終わる。
	test('箱の外へ抜けた玉があればゲームオーバーになる / めり込んだだけでは終わらない', () => {
		const g = newGame('normal', 'lost');
		let over = false;
		g.on('gameOver', () => { over = true; });
		g.start();
		for (let i = 0; i < g.DROP_COOLTIME; i++) g.tick();
		g.drop(200);
		g.tick();
		const bodies = g.engine.world.bodies;
		const body = bodies[bodies.length - 1];

		// 壁 (内側の面は GAME_WIDTH - PLAYAREA_MARGIN、厚さ 100) の中ほどまでめり込ませる。
		// 1 tick では押し戻しきれず、判定の時点でも中心は壁の中にある。
		Matter.Body.setPosition(body, { x: g.GAME_WIDTH - g.PLAYAREA_MARGIN + 60, y: 300 });
		g.tick();
		expect(body.position.x).toBeGreaterThan(g.GAME_WIDTH - g.PLAYAREA_MARGIN);
		expect(over).toBe(false);

		Matter.Body.setPosition(body, { x: g.GAME_WIDTH + 500, y: 300 });
		Matter.Body.setVelocity(body, { x: 0, y: 0 });
		g.tick();
		expect(over).toBe(true);
	});
});

/**
 * mk-go: 途中保存からの早送り (#3192)。コンポーネントから切り出してあるので、
 * 止まる位置・操作の当て方・中止を直接見る。
 */
describe('bubble game fast-forward (#3192)', () => {
	const noWait = { yieldToBrowser: () => Promise.resolve(), now: () => 0, budgetMs: 1e9 };

	test('最後の操作のフレームの tick まで進めて止まり、保持も同じフレームの 2 つの操作も当てる', async () => {
		const g = newGame('normal', 'ff');
		g.start();
		const logs = DropAndFusionGame.deserializeLogs([[40, 1], [0, 0, 200], [60, 0, 100]]);
		const result = await fastForwardGame(g, logs, { ...noWait, isCancelled: () => false });
		expect(result).toBe('done');
		expect(g.frame).toBe(101);
		expect(g.getLogs().map(x => `${x.frame}:${x.operation}`)).toEqual(['40:hold', '40:drop', '100:drop']);
	});

	test('途中でゲームが終わったら gameOver を返す', async () => {
		const g = newGame('normal', 'ff');
		g.start();
		const logs = DropAndFusionGame.deserializeLogs([[40, 0, 200], [10, 2], [100, 0, 100]]);
		const result = await fastForwardGame(g, logs, { ...noWait, isCancelled: () => false });
		expect(result).toBe('gameOver');
		expect(g.frame).toBe(51);
	});

	// 画面を離れると dispose される。続けると壁の無いゲームを回し続ける。
	test('中止されたら進めるのをやめる', async () => {
		const g = newGame('normal', 'ff');
		g.start();
		const logs = DropAndFusionGame.deserializeLogs([[100, 0, 200], [1000, 0, 100]]);
		let yields = 0;
		let t = 0;
		const result = await fastForwardGame(g, logs, {
			// tick 1 回ごとに 1ms 経つ時計と 10ms の枠。1 回目の区切りの後で中止する。
			now: () => t++,
			budgetMs: 10,
			isCancelled: () => yields > 0,
			yieldToBrowser: () => { yields++; return Promise.resolve(); },
		});
		expect(result).toBe('cancelled');
		expect(g.frame).toBeLessThan(1101);
	});

	test('少しずつ進めて進捗を返す', async () => {
		const g = newGame('normal', 'ff');
		g.start();
		const logs = DropAndFusionGame.deserializeLogs([[300, 0, 200]]);
		let t = 0;
		const progress: number[] = [];
		const result = await fastForwardGame(g, logs, {
			// tick 1 回ごとに 1ms 経つ時計と 50ms の枠 = 50 フレームずつ返る。
			now: () => t++,
			budgetMs: 50,
			isCancelled: () => false,
			yieldToBrowser: () => Promise.resolve(),
			onProgress: p => progress.push(p),
		});
		expect(result).toBe('done');
		expect(progress.length).toBeGreaterThan(3);
		expect(progress[progress.length - 1]).toBe(1);
	});
});

describe('bubble game save validation (#3192)', () => {
	beforeEach(() => {
		window.localStorage.clear();
	});

	const put = (l: unknown) => window.localStorage.setItem('mkgo:dropAndFusion:user1:normal', JSON.stringify({ v: 4, m: 'normal', s: '1', l }));

	test('フレーム差が小数の保存は使わない (記録のフレームに一致せず操作が飛ぶ)', () => {
		put([[1.5, 0, 200]]);
		expect(loadDropAndFusionSave('normal')).toBeNull();
	});

	test('x 座標の無い「落とす」操作を含む保存は使わない (NaN の位置に玉ができる)', () => {
		put([[5, 0]]);
		expect(loadDropAndFusionSave('normal')).toBeNull();
		put([[5, 1]]);
		expect(loadDropAndFusionSave('normal')).not.toBeNull();
	});

	test('早送りが終わらないほど長い保存は使わない', () => {
		put([[1e9, 0, 200]]);
		expect(loadDropAndFusionSave('normal')).toBeNull();
	});

	test('期限: backend の 7 日より 1 日早く切る', () => {
		expect(SAVE_MAX_AGE_MS).toBe(6 * 24 * 60 * 60 * 1000);
		const now = 10 * 24 * 60 * 60 * 1000;
		const save = (age: number) => ({ v: 4, m: 'normal', s: String(now - age), l: [] });
		expect(isDropAndFusionSaveExpired(save(SAVE_MAX_AGE_MS - 1), now)).toBe(false);
		expect(isDropAndFusionSaveExpired(save(SAVE_MAX_AGE_MS + 1), now)).toBe(true);
		expect(isDropAndFusionSaveExpired({ v: 4, m: 'normal', s: 'x', l: [] }, now)).toBe(true);
	});
});

/**
 * mk-go: BOUNCY モードと、SPACE / BOUNCY の壁 (#3194)。
 *
 * はみ出しの判定に猶予がある (#3193) と、漂う玉 (space) は壁の上を越え、よく弾む玉
 * (bouncy) は挟まれて角から押し出され、箱の外へ出て消えたままゲームが続いた。
 */
describe('bubble game bouncy / space (#3194)', () => {
	function droppedBody(mode: Mode) {
		const g = newGame(mode, 'seed');
		g.start();
		for (let i = 0; i < g.DROP_COOLTIME; i++) g.tick();
		g.drop(200);
		const bodies = g.engine.world.bodies;
		return bodies[bodies.length - 1];
	}

	test('bouncy はよく弾み、摩擦がほぼ無い。空気抵抗は通常のまま', () => {
		const b = droppedBody('bouncy');
		expect(b.restitution).toBeCloseTo(0.9);
		expect(b.friction).toBeCloseTo(0.01);
		expect(b.frictionStatic).toBe(0);
		expect(b.frictionAir).toBeCloseTo(0.01);
	});

	test('normal の性質は変わらない', () => {
		const b = droppedBody('normal');
		expect(b.restitution).toBeCloseTo(0.2);
		expect(b.friction).toBeCloseTo(0.7);
		expect(b.frictionStatic).toBe(5);
		expect(b.frictionAir).toBeCloseTo(0.01);
	});

	function walls(mode: Mode) {
		const g = newGame(mode, 'walls');
		const statics = g.engine.world.bodies.filter(b => b.label === '_wall_');
		const floor = statics.find(b => b.position.x === g.GAME_WIDTH / 2)!;
		const sides = statics.filter(b => b !== floor);
		return { g, floor, sides };
	}

	// **既存のモードの壁は本家のまま。** 形が変わると版 4 のリプレイと途中保存の結末が変わる。
	test.each(['normal', 'square', 'yen', 'sweets'] as const)('%s の壁は本家のまま', (mode) => {
		const { g, floor, sides } = walls(mode);
		const W = g.GAME_WIDTH, H = g.GAME_HEIGHT, M = g.PLAYAREA_MARGIN, T = 100;
		const box = (b: typeof floor) => [b.bounds.min.x, b.bounds.min.y, b.bounds.max.x, b.bounds.max.y];
		// 本家の組み立て (厚さ 100) そのまま。
		expect(box(floor)).toEqual([0, H - M, W, H - M + T]);
		expect(sides.map(box).sort((a, b) => a[0] - b[0])).toEqual([
			[M - T, 0, M, H],
			[W - M, 0, W - M + T, H],
		]);
	});

	// 角を塞ぐ (床を壁の外まで広げ、壁を床の下まで伸ばす) / 壁を上へ伸ばす。
	test.each(['bouncy', 'space'] as const)('%s は角が塞がり、壁が上へ伸びている', (mode) => {
		const { g, floor, sides } = walls(mode);
		expect(sides).toHaveLength(2);
		for (const w of sides) {
			expect(w.bounds.min.y).toBeLessThanOrEqual(-6000);
			expect(w.bounds.max.y).toBeGreaterThanOrEqual(floor.bounds.max.y);
		}
		// 床はちょうど壁の外側の面まで。短いと角に隙間ができ、長いと壁の外に玉が載る段差になる。
		const outer = [Math.min(...sides.map(w => w.bounds.min.x)), Math.max(...sides.map(w => w.bounds.max.x))];
		expect([floor.bounds.min.x, floor.bounds.max.x]).toEqual(outer);
		// 内側の面は本家と同じ。
		const inner = sides.map(w => (w.position.x < 0 ? w.bounds.max.x : w.bounds.min.x)).sort((a, b) => a - b);
		expect(inner).toEqual([g.PLAYAREA_MARGIN, g.GAME_WIDTH - g.PLAYAREA_MARGIN]);
		expect(floor.bounds.min.y).toBe(g.GAME_HEIGHT - g.PLAYAREA_MARGIN);
	});

	/**
	 * Drops a mono and returns how far it bounces back up after its first contact.
	 * `onBall` drops it on a mono that is already resting.
	 *
	 * **どちらも 2 番目の玉を測る。** 床の場合は 1 番目をホールドして除ける。大きさが違うと
	 * 跳ね方も違うので、比べる意味が無くなる。
	 */
	function rebound(mode: Mode, onBall: boolean) {
		const g = newGame(mode, 'rebound');
		g.start();
		const ticks = (n: number) => { for (let i = 0; i < n; i++) g.tick(); };
		ticks(g.DROP_COOLTIME);
		if (onBall) {
			g.drop(225);
			ticks(300);
		} else {
			g.hold();
		}
		g.drop(225);
		const bodies = g.engine.world.bodies;
		const body = bodies[bodies.length - 1];
		let lowest: number | null = null;
		let top = Infinity;
		for (let i = 0; i < 400; i++) {
			g.tick();
			if (lowest == null) {
				if (body.velocity.y < 0 && body.position.y > 150) lowest = body.position.y;
			} else {
				top = Math.min(top, body.position.y);
			}
		}
		// 跳ね返りを一度も捉えられなかったら 0 ではなく失敗にする (跳ねない、を空振りで満たさない)。
		expect(lowest).not.toBeNull();
		return { height: lowest! - top, size: body.circleRadius, below: onBall ? bodies[bodies.length - 2].circleRadius : null };
	}

	// 物理エンジンは積み重なった玉への衝突で勢いを下の玉と床へ逃がすので、補わないと
	// 落ちている玉に当たってもほとんど跳ねない (実測 1px 未満。床では 169px)。
	test('bouncy: 止まっている玉に当たっても、床と同じくらい跳ねる', () => {
		const floor = rebound('bouncy', false);
		const onBall = rebound('bouncy', true);
		// 同じ玉を比べている。下の玉とは大きさが違う (同じだと合体してしまう)。
		expect(onBall.size).toBe(floor.size);
		expect(onBall.below).not.toBe(onBall.size);
		expect(floor.height).toBeGreaterThan(100);
		expect(onBall.height).toBeGreaterThan(100);
		expect(onBall.height).toBeLessThan(floor.height * 1.5);
	});

	test('normal は止まっている玉に当たってもほとんど跳ねない (補うのは bouncy だけ)', () => {
		expect(rebound('normal', true).height).toBeLessThan(20);
	});

	// よく弾む玉は挟まれると 60px/tick を超える速さで押し出され、その勢いで壁を抜ける。
	// 3 分ぶん回すので、既定の 5 秒では CI のランナーで上限を超える。
	test('bouncy の玉の速さには上限がある', () => {
		const g = newGame('bouncy', 'speed');
		const rng = botRng(9);
		let max = 0;
		g.start();
		while (g.frame < 60 * 60 * 3 && g.tick()) {
			if (g.frame % 35 === 0) g.drop(30 + rng() * 390);
			for (const b of g.engine.world.bodies) {
				if (!b.isStatic) max = Math.max(max, Math.sqrt((b.velocity.x ** 2) + (b.velocity.y ** 2)));
			}
		}
		expect(max).toBeGreaterThan(5);
		expect(max).toBeLessThanOrEqual(15 + 1e-9);
	}, 30000);

	// 上限は bouncy だけ。既存のモードにかけると版 4 の結末が変わる。square はこの局の
	// 1046 フレーム目に 25.8 まで速くなる (実測)。
	test('既存のモードには速さの上限をかけない', () => {
		const g = newGame('square', 'nocap-3');
		const rng = botRng(3);
		let max = 0;
		g.start();
		while (g.frame < 1100 && g.tick()) {
			if (g.frame % 35 === 0) g.drop(30 + rng() * 390);
			for (const b of g.engine.world.bodies) {
				if (!b.isStatic) max = Math.max(max, Math.sqrt((b.velocity.x ** 2) + (b.velocity.y ** 2)));
			}
		}
		// 上限をかけても丸めで 15 をわずかに超えるので、余裕を持たせて見る。
		expect(max).toBeGreaterThan(16);
	});

	// 壁を上へ伸ばさないと、この 2 局で玉が壁の上を越えて外に居続けた (実測)。
	test.each([[8, 'r8'], [22, 'wall-22']] as const)('space: 玉が壁の上を越えて外に居続けない (bot %i)', (bot, seed) => {
		const g = newGame('space', seed);
		const rng = botRng(bot);
		const inner = { l: g.PLAYAREA_MARGIN, r: g.GAME_WIDTH - g.PLAYAREA_MARGIN, b: g.GAME_HEIGHT - g.PLAYAREA_MARGIN };
		const outSince = new Map<number, number>();
		let longestOut = 0;
		g.start();
		for (;;) {
			if (g.frame % 35 === 0) g.drop(30 + rng() * 390);
			const next = g.tick();
			for (const b of g.engine.world.bodies) {
				if (b.isStatic) continue;
				const out = b.bounds.max.x < inner.l || b.bounds.min.x > inner.r || b.bounds.min.y > inner.b;
				if (!out) {
					outSince.delete(b.id);
					continue;
				}
				if (!outSince.has(b.id)) outSince.set(b.id, g.frame);
				longestOut = Math.max(longestOut, g.frame - outSince.get(b.id)!);
			}
			if (!next || g.frame > 60 * 60 * 10) break;
		}
		expect(longestOut).toBeLessThan(30);
	});
});

/**
 * mk-go: 物理を形と別に選ぶ (#3216)。
 */
describe('bubble game physics (#3216)', () => {
	const SHAPES = ['normal', 'square', 'yen', 'sweets'] as const satisfies readonly BaseGameMode[];
	const PHYSICS = ['default', 'bouncy', 'friction'] as const satisfies readonly GamePhysics[];
	const ALL_MODES: Mode[] = [...SHAPES.flatMap(b => PHYSICS.map(p => gameModeOf(b, p))), 'space'];

	test('モードの文字列を形と物理から組み、分解できる', () => {
		for (const base of SHAPES) {
			for (const physics of PHYSICS) {
				expect(parseGameMode(gameModeOf(base, physics))).toEqual({ base, physics });
			}
		}
		expect(gameModeOf('square', 'bouncy')).toBe('square-bouncy');
		expect(gameModeOf('yen', 'default')).toBe('yen');
		// SPACE は物理を選ばない。
		expect(gameModeOf('space', 'bouncy')).toBe('space');
		expect(parseGameMode('space')).toEqual({ base: 'space', physics: 'default' });
		expect(new Set(ALL_MODES).size).toBe(13);
	});

	// #3194 の BOUNCY は NORMAL × BOUNCY。名前を変えるとランキング・ハイスコア・途中保存が切れる。
	test('NORMAL × BOUNCY は #3194 の bouncy のまま', () => {
		expect(gameModeOf('normal', 'bouncy')).toBe('bouncy');
		expect(parseGameMode('bouncy')).toEqual({ base: 'normal', physics: 'bouncy' });
	});

	// 別名を作らない (同じ遊びのランキングが割れる)。知らない文字列は受けない。
	test.each(['normal-bouncy', 'normal-default', 'square-default', 'space-bouncy', 'yen-foo', 'foo-bouncy', 'foo', '', '-bouncy'])('%j はモードではない', (mode) => {
		expect(parseGameMode(mode)).toBeNull();
	});

	test('知らないモードではゲームを作らない', () => {
		expect(() => newGame('space-friction' as Mode, 'x')).toThrow();
	});

	function dropped(mode: Mode) {
		const g = newGame(mode, 'seed');
		g.start();
		for (let i = 0; i < g.DROP_COOLTIME; i++) g.tick();
		g.drop(200);
		const bodies = g.engine.world.bodies;
		return { g, b: bodies[bodies.length - 1] };
	}

	// 物理はどの形にも同じ値で当たる。
	test.each(SHAPES)('%s に物理がそのまま当たる', (base) => {
		const bouncy = dropped(gameModeOf(base, 'bouncy')).b;
		expect(bouncy.restitution).toBeCloseTo(0.9);
		expect(bouncy.friction).toBeCloseTo(0.01);
		const friction = dropped(gameModeOf(base, 'friction'));
		expect(friction.b.restitution).toBe(0);
		expect(friction.b.friction).toBe(1);
		expect(friction.b.frictionStatic).toBe(20);
		// 壁の摩擦はどのモードでも 1 (matter.js が静止した物体の摩擦を 1 に書き換える)。
		// 玉との組は小さい方で決まるので、玉の摩擦がそのまま壁際にも効く。
		for (const w of friction.g.engine.world.bodies.filter(x => x.label === '_wall_')) {
			expect(w.friction).toBe(1);
		}
		const def = dropped(base);
		expect(def.b.restitution).toBeCloseTo(0.2);
		expect(def.b.friction).toBeCloseTo(0.7);
		for (const w of def.g.engine.world.bodies.filter(x => x.label === '_wall_')) {
			expect(w.friction).toBe(1);
		}
	});

	// 角を塞ぐ床と上へ伸ばした壁は BOUNCY の全ての形に当てる。FRICTION は本家の壁のまま。
	test.each(SHAPES)('%s × BOUNCY は壁が上へ伸び、FRICTION は伸びない', (base) => {
		const sideTop = (mode: Mode) => Math.min(...newGame(mode, 'w').engine.world.bodies.filter(b => b.label === '_wall_').map(b => b.bounds.min.y));
		expect(sideTop(gameModeOf(base, 'bouncy'))).toBeLessThanOrEqual(-6000);
		expect(sideTop(gameModeOf(base, 'friction'))).toBe(0);
	});

	/** Frames until a mono dropped right against the left wall reaches the floor. */
	function slideDown(mode: Mode) {
		const { g, b } = (() => {
			const g = newGame(mode, 'wall');
			g.start();
			for (let i = 0; i < g.DROP_COOLTIME; i++) g.tick();
			g.drop(0);
			const bodies = g.engine.world.bodies;
			return { g, b: bodies[bodies.length - 1] };
		})();
		for (let f = 1; f <= 60 * 30; f++) {
			g.tick();
			if (b.bounds.max.y >= g.GAME_HEIGHT - g.PLAYAREA_MARGIN - 1) return f;
		}
		return Infinity;
	}

	// **壁にくっついてゆっくりずり落ちる。最後は下へ届く** (止めきると落とした高さに
	// 貼り付いて、判定領域の中で終わる)。
	// 壁際に寄せて落とすと壁に触れる形 (sweets は定義上の幅より細く、寄せても数 px
	// 離れて触れないので下で別に見る)。
	test.each(['normal', 'square', 'yen'] as const)('FRICTION: %s の壁際の玉はゆっくりずり落ちて、最後は床へ届く', (base) => {
		const def = slideDown(base);
		const friction = slideDown(gameModeOf(base, 'friction'));
		expect(friction).toBeGreaterThan(def * 5);
		expect(friction).toBeLessThan(60 * 30);
	}, 30000);

	/**
	 * Places a mono of `level` with its left edge `gap` px from the left wall at
	 * `angle`, at rest in mid-air, and returns how far it falls in 20 ticks.
	 */
	function fallNearWall(mode: Mode, level: number, gap: number, angle: number) {
		const g = newGame(mode, 'near');
		g.start();
		const mono = g.monoDefinitions.find(m => m.level === level)!;
		const b = (g as unknown as { createBody(m: typeof mono, x: number, y: number): Matter.Body }).createBody(mono, 225, 250);
		Matter.Composite.add(g.engine.world, b);
		Matter.Body.setAngle(b, angle);
		Matter.Body.setPosition(b, { x: b.position.x - (b.bounds.min.x - (g.PLAYAREA_MARGIN + gap)), y: 250 });
		Matter.Body.setVelocity(b, { x: 0, y: 0 });
		const y0 = b.position.y;
		for (let i = 0; i < 20; i++) g.tick();
		return b.position.y - y0;
	}

	// sweets も壁に触れればくっつく。
	test('FRICTION: sweets も壁に触れていればゆっくりずり落ちる', () => {
		expect(fallNearWall('sweets-friction', 3, 0.5, 0)).toBeLessThan(fallNearWall('sweets', 3, 0.5, 0) / 3);
	});

	// **触れていない玉は減速しない。** 外形の幅から余裕を取ると、回転して幅が縮んだ
	// お札 (yen の 8) や sweets が壁から数十 px 離れた空中で減速していた (#3216 のレビュー)。
	test.each([['yen', 8, 40, Math.PI / 2], ['yen', 10, 60, Math.PI / 2], ['sweets', 5, 20, Math.PI / 2], ['square', 5, 20, Math.PI / 4]] as const)('FRICTION: 壁から離れた %s (%i) は空中で減速しない', (base, level, gap, angle) => {
		expect(fallNearWall(gameModeOf(base, 'friction'), level, gap, angle)).toBeCloseTo(fallNearWall(base, level, gap, angle), 5);
	});

	// **壁でくっついた玉には、玉どうしの減速を重ねない。** 重ねると縦の速さがほぼ 0 に
	// なり、壁際に支えの無い玉が宙づりで溜まって早く終わる (壁際に交互に落とす 6 局の
	// 平均: 重ねると 1366 フレーム、重ねないと 1998、DEFAULT は 3275)。
	test('FRICTION: 壁際に積んでも、宙づりの玉が溜まって早く終わらない', () => {
		let total = 0;
		const seeds = ['w1', 'w2', 'w3', 'w4', 'w5', 'w6'];
		for (const seed of seeds) {
			const g = newGame('square-friction', seed);
			let over = false;
			let n = 0;
			g.on('gameOver', () => { over = true; });
			g.start();
			while (!over && g.frame < 60 * 60 * 3) {
				if (g.frame % 35 === 0) g.drop((n++ % 2) * 450);
				if (!g.tick()) break;
			}
			total += g.frame;
		}
		expect(total / seeds.length).toBeGreaterThan(1700);
	}, 60000);

	/** How far a mono dropped slightly off-centre onto a resting mono moves sideways. */
	function sideways(mode: Mode) {
		const g = newGame(mode, 'roll');
		g.start();
		const ticks = (n: number) => { for (let i = 0; i < n; i++) g.tick(); };
		ticks(g.DROP_COOLTIME);
		g.drop(225);
		ticks(120);
		const bodies = g.engine.world.bodies;
		const under = bodies[bodies.length - 1];
		g.drop(225 + (under.circleRadius ?? 0) * 0.5);
		const top = g.engine.world.bodies[g.engine.world.bodies.length - 1];
		const x0 = top.position.x;
		ticks(300);
		return Math.abs(top.position.x - x0);
	}

	test('FRICTION: 玉の上に落とした玉が転がり落ちにくい', () => {
		expect(sideways('normal-friction')).toBeLessThan(sideways('normal') / 2);
	});

	// 他の形は玉が床をすり抜けやすかったので上限を下げた (sweets は精度を落としているので
	// さらに低い)。NORMAL (#3194 の bouncy) は記録とリプレイを変えないため 15 のまま。
	test.each([['bouncy', 15], ['square-bouncy', 12], ['yen-bouncy', 12], ['sweets-bouncy', 10]] as const)('%s の速さの上限は %i', (mode, cap) => {
		const g = newGame(mode, 'cap');
		g.start();
		for (let i = 0; i < g.DROP_COOLTIME; i++) g.tick();
		g.drop(225);
		const b = g.engine.world.bodies[g.engine.world.bodies.length - 1];
		Matter.Body.setVelocity(b, { x: 40, y: 0 });
		g.tick();
		const speed = Math.sqrt((b.velocity.x ** 2) + (b.velocity.y ** 2));
		expect(speed).toBeLessThanOrEqual(cap + 0.5);
		expect(speed).toBeGreaterThan(cap - 1.5);
	});

	// 全ての組み合わせで、同じシードと記録から同じ結末になる (リプレイと途中保存の前提)。
	// **局を短く切って比べる** — 最後まで遊ぶと sweets では CI の既定の上限 (5 秒) を超える。
	test.each(ALL_MODES)('%s: 同じシードと記録から同じ局面になる', (mode) => {
		const run = (logs?: number[][]) => {
			const g = newGame(mode, 'det');
			const rng = botRng(3);
			const replayLogs = logs ? DropAndFusionGame.deserializeLogs(logs) : null;
			let next = 0;
			g.start();
			while (g.frame < 60 * 20) {
				if (replayLogs) {
					while (next < replayLogs.length && replayLogs[next].frame === g.frame) {
						const log = replayLogs[next++];
						if (log.operation === 'drop') g.drop(log.x);
					}
				} else if (g.frame % 35 === 0) {
					g.drop(30 + rng() * 390);
				}
				if (!g.tick()) break;
			}
			const state = g.engine.world.bodies.filter(b => !b.isStatic).map(b => [b.label, b.position.x, b.position.y]);
			return { state, logs: DropAndFusionGame.serializeLogs(g.getLogs()) };
		};
		const first = run();
		expect(run(first.logs).state).toEqual(first.state);
	}, 30000);

	// 跳ね返りの補いは BOUNCY の全ての形に当てる。補わないと玉の上ではほとんど跳ねない
	// (NORMAL の実測で 1px 未満)。
	test.each(['square-bouncy', 'yen-bouncy'] as const)('%s: 止まっている玉の上でも跳ねる', (mode) => {
		const g = newGame(mode, 'rebound');
		g.start();
		const ticks = (n: number) => { for (let i = 0; i < n; i++) g.tick(); };
		ticks(g.DROP_COOLTIME);
		g.drop(225);
		ticks(300);
		g.drop(225);
		const body = g.engine.world.bodies[g.engine.world.bodies.length - 1];
		let lowest: number | null = null;
		let top = Infinity;
		for (let i = 0; i < 400; i++) {
			g.tick();
			if (lowest == null) {
				if (body.velocity.y < 0 && body.position.y > 150) lowest = body.position.y;
			} else {
				top = Math.min(top, body.position.y);
			}
		}
		expect(lowest).not.toBeNull();
		expect(lowest! - top).toBeGreaterThan(50);
	});

	/**
	 * Plays a short fixed game and returns frame / score / a digest of every
	 * mono's position.
	 */
	function goldenDigest(mode: Mode) {
		const g = newGame(mode, 'golden');
		const rng = botRng(7);
		let score = 0;
		let over = false;
		g.on('changeScore', v => { score = v; });
		g.on('gameOver', () => { over = true; });
		g.start();
		while (!over && g.frame < 60 * 15) {
			if (g.frame % 35 === 0) g.drop(30 + rng() * 390);
			if (g.frame % 175 === 100) g.hold();
			if (!g.tick()) break;
		}
		const state = g.engine.world.bodies.filter(b => !b.isStatic).map(b => `${b.label}:${b.position.x.toFixed(4)},${b.position.y.toFixed(4)}`).join('|');
		let h = 0x811c9dc5;
		for (let i = 0; i < state.length; i++) {
			h ^= state.charCodeAt(i);
			h = Math.imul(h, 0x01000193) >>> 0;
		}
		return `${g.frame}/${score}/${h.toString(16)}`;
	}

	// **既存のモードは #3216 の前と同じ局面になる。** 上の決定性テストは同じビルドの中で
	// リプレイと比べるだけなので、既存の物理が変わっても両方が一緒に変わって緑のまま
	// 通る。値は #3216 の前のエンジン (HEAD の game.ts) で計った (版 4 のリプレイと
	// 途中保存が同じ結末になる前提)。**値を書き換えるなら VERSION を上げること。**
	test.each([
		['normal', '900/113/a4615903'],
		['square', '900/58/1ed1cff5'],
		['yen', '900/407/41f248c8'],
		['sweets', '900/1520/f11decf3'],
		['space', '900/92/bb2ef10d'],
		['bouncy', '900/57/d2baaf39'],
	] as const)('%s は #3216 の前と同じ局面になる', (mode, expected) => {
		expect(goldenDigest(mode)).toBe(expected);
	}, 30000);

	test('単位とモード名は形と物理から決まる', () => {
		expect(dropAndFusionScoreUnit('yen-bouncy')).toBe('円');
		expect(dropAndFusionScoreUnit('sweets-friction')).toBe('kcal');
		expect(dropAndFusionScoreUnit('bouncy')).toBe('pt');
		expect(dropAndFusionModeLabel('square-friction')).toBe('SQUARE × FRICTION');
		expect(dropAndFusionModeLabel('bouncy')).toBe('NORMAL × BOUNCY');
		expect(dropAndFusionModeLabel('yen')).toBe('YEN');
	});
});

/**
 * mk-go: 対戦のおじゃま石と攻撃 (#3229)。
 */
describe('bubble game versus (#3229)', () => {
	type Internals = {
		createBody(m: unknown, x: number, y: number, stone?: boolean): Matter.Body;
		fusion(a: Matter.Body, b: Matter.Body): void;
		fusionReadyBodyIds: number[];
	};

	function newVersus(mode: Mode, seed: string) {
		return new DropAndFusionGame({ seed, gameMode: mode, getMonoRenderOptions: () => ({}), versus: true });
	}

	function internals(g: DropAndFusionGame) {
		return g as unknown as Internals;
	}

	function place(g: DropAndFusionGame, level: number, x: number, y: number) {
		const mono = g.monoDefinitions.find(m => m.level === level)!;
		const b = internals(g).createBody(mono, x, y);
		Matter.Composite.add(g.engine.world, b);
		internals(g).fusionReadyBodyIds.push(b.id);
		return b;
	}

	function placeStone(g: DropAndFusionGame, x: number, y: number) {
		const lv = g.monoDefinitions.find(m => m.level === VERSUS_RULES.stoneLevel)!;
		const b = internals(g).createBody({ ...lv, shape: 'circle', sizeY: lv.sizeX }, x, y, true);
		Matter.Composite.add(g.engine.world, b);
		return b;
	}

	function stones(g: DropAndFusionGame) {
		return g.engine.world.bodies.filter(b => b.label === DropAndFusionGame.STONE_LABEL);
	}

	function attacks(g: DropAndFusionGame) {
		const got: number[] = [];
		g.on('attack', n => got.push(n));
		return got;
	}

	function radiusOf(g: DropAndFusionGame, level: number) {
		return g.monoDefinitions.find(m => m.level === level)!.sizeX / 2;
	}

	// 合体した玉の大きさでは送らない (#3231)。単発の合体はどの Lv でも送らない。
	test.each([[1, []], [2, []], [3, []], [10, []]] as const)('Lv%i どうしの合体は %j を送る', (level, want) => {
		const g = newVersus('normal', 's');
		g.start();
		const got = attacks(g);
		const r = radiusOf(g, level);
		internals(g).fusion(place(g, level, 150, 300), place(g, level, 150 + (2 * r), 300));
		expect(got).toEqual(want);
	});

	// コンボは 2 コンボ目から 1 コンボごとに +1。
	test('2 コンボ目から 1 つずつ増える', () => {
		const g = newVersus('normal', 's');
		g.start();
		const got = attacks(g);
		for (const x of [80, 200, 320]) {
			const r = radiusOf(g, 3);
			internals(g).fusion(place(g, 3, x, 300), place(g, 3, x + (2 * r), 300));
		}
		// 1 コンボ目は送らず、2 コンボ目が 1、3 コンボ目が 2。
		expect(got).toEqual([1, 2]);
	});

	// 合体した玉に触れている石は消え、2 個で 1 個を送る。端数は持ち越す。
	test('触れた石を消し、2 個で 1 個を送る (端数は持ち越す)', () => {
		const g = newVersus('normal', 's');
		g.start();
		const got = attacks(g);
		const cleared: number[] = [];
		g.on('stonesCleared', n => cleared.push(n));
		const r1 = radiusOf(g, 1);
		const rs = radiusOf(g, VERSUS_RULES.stoneLevel);

		// 1 回目: 石 1 個に触れる。単発の合体 (0) + 石 1 個 (端数) = 攻撃なし。
		const a = place(g, 1, 100, 300);
		const b = place(g, 1, 100 + (2 * r1), 300);
		placeStone(g, 100 - r1 - rs, 300);
		// 離れた石は消えない。
		const far = placeStone(g, 400, 100);
		internals(g).fusion(a, b);
		expect(cleared).toEqual([1]);
		expect(got).toEqual([]);
		expect(stones(g)).toEqual([far]);

		// 2 回目: もう 1 個消すと、持ち越した 1 個と合わせて 1 個を送る。
		const c = place(g, 1, 300, 450);
		const d = place(g, 1, 300 + (2 * r1), 450);
		placeStone(g, 300 + (2 * r1) + r1 + rs, 450);
		g.frame += 1000; // コンボにしない
		internals(g).fusion(c, d);
		expect(cleared).toEqual([1, 1]);
		expect(got).toEqual([1]);
	});

	// 石どうしは同じラベルでも合体しない。
	test('石どうしは合体しない', () => {
		const g = newVersus('normal', 's');
		g.start();
		const rs = radiusOf(g, VERSUS_RULES.stoneLevel);
		placeStone(g, 200, 500);
		placeStone(g, 200 + (2 * rs) - 1, 500);
		for (let i = 0; i < 30; i++) g.tick();
		expect(stones(g)).toHaveLength(2);
	});

	// 届いた石は、次に玉を落とした後に降る。一度に最大 5 個で、残りは次の手番。
	test('受け取った石は落とした後に最大 5 個ずつ降り、記録に残る', () => {
		const g = newVersus('normal', 's');
		g.start();
		const pending: number[] = [];
		g.on('changePendingGarbage', n => pending.push(n));
		g.receiveAttack(7);
		expect(g.pendingGarbage).toBe(7);
		expect(stones(g)).toHaveLength(0);

		for (let i = 0; i < g.DROP_COOLTIME; i++) g.tick(); // 落とせるまで待つ
		expect(stones(g)).toHaveLength(0);
		g.drop(100);
		expect(stones(g)).toHaveLength(VERSUS_RULES.maxGarbagePerDrop);
		expect(g.pendingGarbage).toBe(2);
		// 石は同じ位置に重ねて出さない (降った直後に見る。進めると散らばって分からない)。
		expect(new Set(stones(g).map(b => Math.round(b.position.x))).size).toBe(VERSUS_RULES.maxGarbagePerDrop);
		for (let i = 0; i < 40; i++) g.tick();
		g.drop(300);
		expect(stones(g)).toHaveLength(7);
		expect(g.pendingGarbage).toBe(0);
		expect(pending).toEqual([7, 2, 0]);
		expect(g.getLogs().filter(l => l.operation === 'garbage').map(l => l.operation === 'garbage' && l.count)).toEqual([5, 2]);
	});

	// 石は今落とした玉と重ならない位置 (玉の上端より上) に出る。重なると玉を弾いて
	// 狙った位置をずらす。
	test.each(['normal', 'square', 'yen', 'sweets', 'space'] as const)('%s: 石は落とした玉と重ならない', (mode) => {
		for (const x of [30, 120, 225, 330, 420]) {
			const g = newVersus(mode, `overlap-${x}`);
			g.start();
			for (let i = 0; i < g.DROP_COOLTIME; i++) g.tick();
			g.receiveAttack(5);
			g.drop(x);
			const dropped = g.engine.world.bodies.find(b => !b.isStatic && !b.isSensor && b.label !== DropAndFusionGame.STONE_LABEL)!;
			for (const s of stones(g)) {
				expect(Matter.Collision.collides(s, dropped)).toBeNull();
			}
		}
	});

	// SPACE でも石ははみ出しの判定領域から抜ける (重力が弱いので、玉と同じく下向きの
	// 力が要る)。
	test('space: 降った石はすぐに判定領域 (y < 100) を抜ける', () => {
		const g = newVersus('space', 'space-stones');
		g.start();
		for (let i = 0; i < g.DROP_COOLTIME; i++) g.tick();
		g.receiveAttack(5);
		g.drop(225);
		const grace = g.msToFrame(g.OVERFLOW_GRACE_MS);
		for (let i = 0; i < grace / 2; i++) g.tick();
		for (const s of stones(g)) expect(s.position.y).toBeGreaterThan(100);
	});

	// 相手から届く数は検査する。NaN は予告を壊し、以後の攻撃も消す。
	test('受け取る数が壊れていても予告は壊れない', () => {
		const g = newVersus('normal', 's');
		g.start();
		g.receiveAttack(Number.NaN);
		g.receiveAttack(Number.POSITIVE_INFINITY);
		g.receiveAttack(-3);
		expect(g.pendingGarbage).toBe(0);
		g.receiveAttack(3);
		expect(g.pendingGarbage).toBe(3);
		g.receiveAttack(1e9);
		expect(g.pendingGarbage).toBe(VERSUS_RULES.maxPendingGarbage);
	});

	// 対戦かどうかは記録に残らないので、対戦の指定をせずに作ったゲームでも、記録の
	// garbage を当てれば同じ位置に石が降る。
	test('対戦の指定が無くても、記録の garbage で同じ位置に石が降る', () => {
		const logs = [{ frame: 40, operation: 'drop' as const, x: 200 }, { frame: 40, operation: 'garbage' as const, count: 4 }];

		function run(versus: boolean) {
			const g = new DropAndFusionGame({ seed: 'rep', gameMode: 'normal', getMonoRenderOptions: () => ({}), versus });
			g.start();
			let next = 0;
			while (g.frame <= 40) {
				while (next < logs.length && logs[next].frame === g.frame) g.applyLog(logs[next++]);
				g.tick();
			}
			return stones(g).map(b => [Math.round(b.position.x), Math.round(b.position.y)]);
		}

		expect(run(false)).toHaveLength(4);
		expect(run(false)).toEqual(run(true));
	});

	// 石が消える (合体) ところまで含めて、対戦の指定なしのリプレイが実際の対局と
	// 同じ結末になる。消し方が指定に依存していると、石は同じ位置に降るのに消えずに
	// 残ってずれる (#3229 のレビュー 2 周目で実測)。
	test.each(['a', 'b', 'c'])('対戦の指定なしのリプレイも同じ結末になる (%s)', (seed) => {
		const live = newVersus('normal', `live-${seed}`);
		const rng = botRng(seed.charCodeAt(0));
		let liveScore = 0;
		let liveCleared = 0;
		let over = false;
		live.on('changeScore', v => { liveScore = v; });
		live.on('stonesCleared', n => { liveCleared += n; });
		live.on('gameOver', () => { over = true; });
		live.start();
		while (!over && live.frame < 60 * 60 * 2) {
			if (live.frame % 35 === 0) live.drop(30 + rng() * 390);
			if (live.frame % 40 === 0) live.receiveAttack(1 + Math.floor(rng() * 5));
			if (!live.tick()) break;
		}
		expect(liveCleared).toBeGreaterThan(0);

		const logs = DropAndFusionGame.deserializeLogs(DropAndFusionGame.serializeLogs(live.getLogs()));
		const g = newGame('normal', `live-${seed}`);
		let score = 0;
		g.on('changeScore', v => { score = v; });
		g.start();
		let next = 0;
		while (g.frame < live.frame) {
			while (next < logs.length && logs[next].frame === g.frame) g.applyLog(logs[next++]);
			if (!g.tick()) break;
		}
		expect(g.frame).toBe(live.frame);
		expect(score).toBe(liveScore);
		expect(stones(g)).toHaveLength(stones(live).length);
	}, 30000);

	// 記録から来る数も上限で頭打ちにする (壊れた途中保存でタブを固めない)。
	test('記録の garbage も一度に降る上限を超えない', () => {
		const g = newGame('normal', 's');
		g.start();
		g.applyLog({ frame: 0, operation: 'garbage', count: 1e7 });
		expect(stones(g)).toHaveLength(VERSUS_RULES.maxGarbagePerDrop);
	});

	// 終わった盤面には石が届かない (予告も増えない)。
	test('ゲームオーバーの後は石を受け取らない', () => {
		const g = newVersus('normal', 's');
		g.start();
		g.surrender();
		g.receiveAttack(3);
		expect(g.pendingGarbage).toBe(0);
	});

	// 1 人用は変わらない: 攻撃は出ず、石も届かない。
	test('1 人用では攻撃も石も出ない', () => {
		const g = newGame('normal', 's');
		g.start();
		const got = attacks(g);
		const r = radiusOf(g, 3);
		// 対戦なら 1 と 2 を送る 3 連続の合体。
		for (const x of [80, 200, 320]) {
			internals(g).fusion(place(g, 3, x, 300), place(g, 3, x + (2 * r), 300));
		}
		g.receiveAttack(5);
		for (let i = 0; i < g.DROP_COOLTIME; i++) g.tick();
		g.drop(100);
		expect(got).toEqual([]);
		expect(g.pendingGarbage).toBe(0);
		expect(stones(g)).toHaveLength(0);
	});

	// 同じシードと記録から、石を含めて同じ結末になる (受け手の記録だけで再現できる)。
	test('石を含めて同じシードと記録から同じ結末になる', () => {
		function play() {
			const g = newVersus('normal', 'versus-seed');
			const rng = botRng(7);
			let score = 0;
			let over = false;
			g.on('changeScore', v => { score = v; });
			g.on('gameOver', () => { over = true; });
			g.start();
			while (!over && g.frame < 60 * 60 * 2) {
				if (g.frame % 35 === 0) g.drop(30 + rng() * 390);
				// 相手からの攻撃が不規則に届く。
				if (g.frame % 97 === 0) g.receiveAttack(1 + Math.floor(rng() * 4));
				if (!g.tick()) break;
			}
			return { g, score, frame: g.frame };
		}

		const first = play();
		const serialized = DropAndFusionGame.serializeLogs(first.g.getLogs());
		expect(serialized.some(l => l[1] === 3)).toBe(true);

		const logs = DropAndFusionGame.deserializeLogs(serialized);
		const g = newVersus('normal', 'versus-seed');
		let score = 0;
		g.on('changeScore', v => { score = v; });
		g.start();
		let next = 0;
		while (g.frame < first.frame) {
			while (next < logs.length && logs[next].frame === g.frame) g.applyLog(logs[next++]);
			if (!g.tick()) break;
		}
		expect(g.frame).toBe(first.frame);
		expect(score).toBe(first.score);
		const pos = (x: DropAndFusionGame) => stones(x).map(b => [Math.round(b.position.x), Math.round(b.position.y)]);
		expect(pos(g)).toEqual(pos(first.g));
		expect(DropAndFusionGame.serializeLogs(g.getLogs())).toEqual(serialized);
	}, 30000);

	// 石の位置は専用の乱数で決める。玉の順番の乱数と共有すると、石が降るたびに自分の
	// 玉の順番が変わる (同じシードで遊ぶ両者の順番がずれる)。
	test('石が降っても自分の玉の順番は変わらない', () => {
		function sequence(withGarbage: boolean) {
			const g = newVersus('normal', 'seq');
			// 落とす直前の「次の玉」を並べる (monoAdded は合体でできた玉でも出るので、
			// 石で合体が変わると列が変わる)。
			const seen: string[] = [];
			let stock: { mono: { id: string } }[] = [];
			g.on('changeStock', v => { stock = v; });
			g.start();
			for (let i = 0; i < 8; i++) {
				for (let t = 0; t < g.DROP_COOLTIME; t++) g.tick();
				if (withGarbage) g.receiveAttack(3);
				seen.push(stock[0].mono.id);
				g.drop(225);
			}
			return seen;
		}

		const plain = sequence(false);
		expect(sequence(true)).toEqual(plain);
		// 並びが自明に一致する (全部同じ玉) のでは確かめにならない。
		expect(new Set(plain).size).toBeGreaterThan(1);
	});

	test('記録の garbage は直列化して戻せる', () => {
		const logs = [{ frame: 10, operation: 'drop' as const, x: 100 }, { frame: 10, operation: 'garbage' as const, count: 3 }];
		expect(DropAndFusionGame.deserializeLogs(DropAndFusionGame.serializeLogs(logs))).toEqual(logs);
	});
});

/**
 * mk-go (#3232): 対戦の記録は、遊んだ版のエンジンでしか同じ結末に再生できない。
 * 記録には版を残し、リプレイは版が違えば再生しない。**だから対戦のルールを変えたら
 * 版を上げること。** 版を上げずにルールだけ変えると、古い記録が別の結末で再生される
 * (しかも版が一致しているので止められない)。
 *
 * 版ごとに「その版のルール」を下の表で持ち、今の版の行と食い違えば落ちる。ルールを
 * 変えるには、VERSION を上げて新しい行を足すしかない。**既にある行は書き換えない**
 * (その版で遊ばれた記録の前提そのもの)。
 */
describe('versus rules are pinned per engine version (mk-go #3232)', () => {
	type Outcome = { attackTotal: number; score: number; frame: number };
	// 形と物理が一通り出るモード。物理 (BOUNCY / FRICTION / SPACE) や玉の定義を
	// 版を上げずに変えたときも落ちるように、normal だけにしない。
	const MODES = ['normal', 'bouncy', 'square-friction', 'yen', 'sweets-bouncy', 'space'] as const;
	const RULES_BY_VERSION: Record<number, { rules: typeof VERSUS_RULES; outcomes: Record<typeof MODES[number], Outcome> }> = {
		4: {
			rules: { maxGarbagePerDrop: 5, maxPendingGarbage: 200, stonesPerAttack: 2, stoneLevel: 2, touchMargin: 2 },
			outcomes: {
				'normal': { attackTotal: 212, score: 755, frame: 3209 },
				'bouncy': { attackTotal: 273, score: 1772, frame: 4636 },
				'square-friction': { attackTotal: 109, score: 602, frame: 2962 },
				'yen': { attackTotal: 173, score: 3409, frame: 3352 },
				'sweets-bouncy': { attackTotal: 853, score: 23500, frame: 6642 },
				'space': { attackTotal: 28, score: 242, frame: 1687 },
			},
		},
	};

	// 攻撃の式 (コンボの間隔・石の換算) と物理は定数の表には出ないので、決まった
	// 場面で対局を回したときの「送った攻撃の合計」と得点・終わったフレームでも固定する。
	function play(mode: typeof MODES[number]): Outcome {
		const g = new DropAndFusionGame({ seed: 'rules-pin', gameMode: mode, getMonoRenderOptions: () => ({}), versus: true });
		const rng = botRng(11);
		let attackTotal = 0;
		let score = 0;
		let over = false;
		g.on('attack', (n: number) => { attackTotal += n; });
		g.on('changeScore', (v: number) => { score = v; });
		g.on('gameOver', () => { over = true; });
		g.start();
		while (!over && g.frame < 60 * 60 * 2) {
			if (g.frame % 35 === 0) g.drop(30 + rng() * 390);
			if (g.frame % 97 === 0) g.receiveAttack(1 + Math.floor(rng() * 4));
			if (!g.tick()) break;
		}
		return { attackTotal, score, frame: g.frame };
	}

	test('今の版の行がある', () => {
		expect(RULES_BY_VERSION[DropAndFusionGame.VERSION]).toBeDefined();
	});

	test('対戦の定数が今の版の行と一致する', () => {
		expect({ ...VERSUS_RULES }).toEqual(RULES_BY_VERSION[DropAndFusionGame.VERSION].rules);
	});

	test('攻撃の式と物理が今の版の行と一致する', () => {
		const want = RULES_BY_VERSION[DropAndFusionGame.VERSION].outcomes;
		const got = Object.fromEntries(MODES.map(m => [m, play(m)]));
		expect(got).toEqual(want);
	}, 120000);
});

// mk-go (#3232): 対戦のリプレイは記録から盤面を進める。対局と同じシードと記録から
// 同じ結末になること。操作を当てるフレームが 1 つずれるだけで別の結末になる。
describe('versus replay cursor (mk-go #3232)', () => {
	function newVersusGame(seed: string) {
		return new DropAndFusionGame({ seed, gameMode: 'normal', getMonoRenderOptions: () => ({}), versus: true });
	}

	function playOriginal(seed: string) {
		const g = newVersusGame(seed);
		const rng = botRng(5);
		let score = 0;
		let over = false;
		g.on('changeScore', (v: number) => { score = v; });
		g.on('gameOver', () => { over = true; });
		g.start();
		while (!over && g.frame < 60 * 60 * 2) {
			// 同じフレームに保持と投下を重ねる (同じフレームの操作を全部当てるかを見る)。
			if (g.frame % 140 === 0) g.hold();
			if (g.frame % 35 === 0) g.drop(30 + rng() * 390);
			if (g.frame % 97 === 0) g.receiveAttack(1 + Math.floor(rng() * 4));
			if (!g.tick()) break;
		}
		return { g, score, frame: g.frame };
	}

	const stonesOf = (g: DropAndFusionGame) => g.engine.world.bodies
		.filter(b => b.label === DropAndFusionGame.STONE_LABEL)
		.map(b => [Math.round(b.position.x), Math.round(b.position.y)]);

	test('対局と同じ記録から同じ結末になる (少しずつ進めても)', () => {
		const first = playOriginal('replay-cursor');
		const logs = DropAndFusionGame.deserializeLogs(DropAndFusionGame.serializeLogs(first.g.getLogs()));
		const g = newVersusGame('replay-cursor');
		let score = 0;
		g.on('changeScore', (v: number) => { score = v; });
		g.start();
		const cursor = createReplayCursor(g, logs, first.frame);
		let frame = 0;
		while (cursor.advanceTo(frame += 7)) { /* 7 フレームずつ */ }
		expect(cursor.done).toBe(true);
		expect(g.frame).toBe(first.frame);
		expect(score).toBe(first.score);
		expect(stonesOf(g)).toEqual(stonesOf(first.g));
	}, 60000);

	test('終わったフレーム (時間切れ) で止まる', () => {
		const first = playOriginal('replay-cursor');
		const logs = DropAndFusionGame.deserializeLogs(DropAndFusionGame.serializeLogs(first.g.getLogs()));
		const g = newVersusGame('replay-cursor');
		g.start();
		const cursor = createReplayCursor(g, logs, 500);
		expect(cursor.advanceTo(400)).toBe(true);
		expect(g.frame).toBe(400);
		expect(cursor.advanceTo(10_000)).toBe(false);
		expect(g.frame).toBe(500);
	}, 60000);
});
