/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { EventEmitter } from 'eventemitter3';
import * as Matter from 'matter-js';
import seedrandom from 'seedrandom';
import { NORAML_MONOS, SQUARE_MONOS, SWEETS_MONOS, YEN_MONOS } from './monos.js';

export type Mono = {
	id: string;
	level: number;
	sizeX: number;
	sizeY: number;
	shape: 'circle' | 'rectangle' | 'custom';
	vertices?: Matter.Vector[][];
	verticesSize?: number;
	score: number;
	dropCandidate: boolean;
};

/**
 * The shape set (mono definitions) of the game (mk-go, #3216).
 */
export type BaseGameMode = 'normal' | 'yen' | 'square' | 'sweets' | 'space';

/**
 * The body physics, chosen separately from the shape set (mk-go, #3216).
 * space has its own physics and takes no variant.
 */
export type GamePhysics = 'default' | 'bouncy' | 'friction';

type VariantBase = Exclude<BaseGameMode, 'space'>;
type VariantPhysics = Exclude<GamePhysics, 'default'>;

/**
 * The game mode string. It keys everything a game is stored under (ranking,
 * high score, save, replay), so each combination of shape set and physics is
 * a mode of its own.
 *
 * mk-go (#3216): 形と物理をつないだ 1 つの文字列にする。サーバーはこの文字列を
 * 検査せずにそのまま記録・集計するので、backend を変えずにランキングが組み合わせ
 * ごとに分かれる。**NORMAL × BOUNCY だけは `bouncy` のまま** — #3194 で独立の
 * モードとして出したときの名前で、これまでのランキング・ハイスコア・途中保存が
 * この名前で残っている。
 */
export type GameMode = BaseGameMode | 'bouncy' | `${VariantBase}-${VariantPhysics}`;

const BASE_GAME_MODES: readonly BaseGameMode[] = ['normal', 'yen', 'square', 'sweets', 'space'];
const GAME_PHYSICS: readonly GamePhysics[] = ['default', 'bouncy', 'friction'];

/**
 * Builds the game mode string from a shape set and physics (mk-go, #3216).
 */
export function gameModeOf(base: BaseGameMode, physics: GamePhysics): GameMode {
	if (base === 'space' || physics === 'default') return base;
	if (base === 'normal' && physics === 'bouncy') return 'bouncy';
	return `${base}-${physics}`;
}

/**
 * Splits a game mode string into its shape set and physics (mk-go, #3216).
 * Returns null for a string that is not a game mode.
 */
export function parseGameMode(mode: string): { base: BaseGameMode; physics: GamePhysics } | null {
	if (mode === 'bouncy') return { base: 'normal', physics: 'bouncy' };
	if ((BASE_GAME_MODES as readonly string[]).includes(mode)) return { base: mode as BaseGameMode, physics: 'default' };
	const i = mode.lastIndexOf('-');
	if (i < 0) return null;
	const base = mode.slice(0, i);
	const physics = mode.slice(i + 1);
	if (base === 'space' || !(BASE_GAME_MODES as readonly string[]).includes(base)) return null;
	if (physics === 'default' || !(GAME_PHYSICS as readonly string[]).includes(physics)) return null;
	// normal-bouncy は bouncy と同じ遊びなので、別名を作らない (ランキングが割れる)。
	if (base === 'normal' && physics === 'bouncy') return null;
	return { base: base as BaseGameMode, physics: physics as GamePhysics };
}

export type Log = {
	frame: number;
	operation: 'drop';
	x: number;
} | {
	frame: number;
	operation: 'hold';
} | {
	frame: number;
	operation: 'surrender';
} | {
	// mk-go (#3229): 対戦で、相手から届いたおじゃま石が降った。石は受け手の盤面で
	// 降らせるので、受け手の記録に残す (リプレイは「シード + 記録」だけで再現できる)。
	// 1 人用の記録には現れない。
	frame: number;
	operation: 'garbage';
	count: number;
};

/**
 * The rules of the versus mode (mk-go, #3229).
 */
export const VERSUS_RULES = {
	// 一度に降るおじゃま石の上限。残りは次の手番に回す。一度に大量に降ると勝負が
	// 1 回で決まりやすく、物理演算も荒れる。
	maxGarbagePerDrop: 5,
	// 予告に積める石の上限 (壊れた値や改造した相手から、終わらない数が届かないように)。
	// 盤面はこれよりずっと少ない数で埋まるので、遊びには影響しない。
	maxPendingGarbage: 200,
	// 消した石 N 個で 1 個を相手に送る (端数は持ち越す)。
	stonesPerAttack: 2,
	// おじゃま石の大きさ (この Lv の玉と同じ)。
	stoneLevel: 2,
	// 合体した玉と石の間がこの距離 (px) 以内なら「触れている」とみなす。玉は積み
	// 上がった状態でも物理演算の上ではわずかに離れていることがあるので、0 にすると
	// 見た目は触れているのに消えない。
	touchMargin: 2,
} as const;

export class DropAndFusionGame extends EventEmitter<{
	changeScore: (newScore: number) => void;
	changeCombo: (newCombo: number) => void;
	changeStock: (newStock: { id: string; mono: Mono }[]) => void;
	changeHolding: (newHolding: { id: string; mono: Mono } | null) => void;
	dropped: (x: number) => void;
	fusioned: (x: number, y: number, nextMono: Mono | null, scoreDelta: number) => void;
	collision: (energy: number, bodyA: Matter.Body, bodyB: Matter.Body) => void;
	monoAdded: (mono: Mono) => void;
	/**
	 * Emitted when a mono starts or stops overflowing the box (mk-go, #3193).
	 * `true` while at least one mono stays in the overflow area.
	 */
	overflowWarning: (overflowing: boolean) => void;
	/**
	 * Emitted in the versus mode when this board sends stones to the opponent
	 * (mk-go, #3229). **リプレイ中も出る** (対戦の指定をして作った場合) ので、相手へ
	 * 送るのは実際の対局のときだけにすること。
	 */
	attack: (count: number) => void;
	/**
	 * Emitted in the versus mode when the number of stones waiting to fall on
	 * this board changes (mk-go, #3229).
	 */
	changePendingGarbage: (count: number) => void;
	/**
	 * Emitted when stones are cleared by a fusion (mk-go, #3229).
	 */
	stonesCleared: (count: number) => void;
	gameOver: () => void;
}> {
	private PHYSICS_QUALITY_FACTOR = 16; // 低いほどパフォーマンスが高いがガタガタして安定しなくなる、逆に高すぎても何故か不安定になる
	private COMBO_INTERVAL = 60; // frame
	// mk-go: 4 = はみ出しの判定を「判定領域に 2 秒とどまったら」に変えた (#3193)。
	// 同じ操作の記録でも結果が変わるので、版を分けないと古いリプレイや途中保存が
	// 別の結末になる。**static も持つ** — 途中保存 (#3192) の版をゲームを作る前に
	// 比べるため。
	public static readonly VERSION = 4;
	public readonly GAME_VERSION = DropAndFusionGame.VERSION;
	public readonly GAME_WIDTH = 450;
	public readonly GAME_HEIGHT = 600;
	public readonly DROP_COOLTIME = 30; // frame
	public readonly PLAYAREA_MARGIN = 25;
	private STOCK_MAX = 4;
	/**
	 * How long a mono may stay in the overflow area before the game is over
	 * (mk-go, #3193).
	 */
	public readonly OVERFLOW_GRACE_MS = 2000;
	private WALL_THICKNESS = 100;
	// mk-go (#3194): bouncy / space で左右の壁を上へ伸ばす高さ。理由は constructor の
	// 壁の組み立てを参照。space は空気抵抗が 0 で重力が弱いので、玉が上へ弾かれると
	// 高く上がる (上向きの速さ 5.34 で約 3800。実際のプレイで出た最大値)。余裕を持たせる。
	private CONTAINING_WALL_EXTRA_HEIGHT = 6000;
	private TICK_DELTA = 1000 / 60; // 60fps

	public frame = 0;
	public engine: Matter.Engine;
	private tickCallbackQueue: { frame: number; callback: () => void; }[] = [];
	private overflowCollider: Matter.Body;
	private isGameOver = false;
	private gameMode: GameMode;
	// mk-go (#3216): gameMode を形と物理に分けたもの。物理の判定はこちらを見る。
	private baseMode: BaseGameMode;
	private physics: GamePhysics;
	private rng: () => number;
	private logs: Log[] = [];

	/**
	 * フィールドに出ていて、かつ合体の対象となるアイテム
	 */
	private fusionReadyBodyIds: Matter.Body['id'][] = [];

	private gameOverReadyBodyIds: Matter.Body['id'][] = [];

	/**
	 * mk-go (#3193): 判定領域に入った玉と、入ったフレーム。出たら (合体して消えたら) 消す。
	 */
	private overflowingSince = new Map<Matter.Body['id'], number>();
	private overflowing = false;

	/**
	 * fusion予約アイテムのペア
	 * TODO: これらのモノは光らせるなどの演出をすると視覚的に楽しそう
	 */
	private fusionReservedPairs: { bodyA: Matter.Body; bodyB: Matter.Body }[] = [];

	private latestDroppedAt = 0; // frame
	private latestFusionedAt = 0; // frame
	private stock: { id: string; mono: Mono }[] = [];
	private holding: { id: string; mono: Mono } | null = null;

	// mk-go (#3229): 石の降る位置の乱数。玉の順番 (rng) とは分ける — 共有すると、石が
	// 降るたびに自分の玉の順番が変わる。**対戦でなくても作る** — リプレイは記録の
	// garbage を当てるだけで石を降らせる (対戦かどうかは記録に残らないので、作る側が
	// 指定を忘れても石の位置がずれないようにする)。
	private garbageRng: () => number;

	// mk-go (#3229): 対戦。null なら 1 人用 (攻撃も予告も出ない)。
	private versus: {
		// 相手から届いて、まだ降っていない石の数。
		pending: number;
		// 消した石の数の端数 (stonesPerAttack に満たない分)。
		clearedCarry: number;
	} | null = null;

	public get monoDefinitions() {
		switch (this.baseMode) {
			case 'normal': return NORAML_MONOS;
			case 'yen': return YEN_MONOS;
			case 'square': return SQUARE_MONOS;
			case 'sweets': return SWEETS_MONOS;
			case 'space': return NORAML_MONOS;
		}
	}

	private _combo = 0;
	private get combo() {
		return this._combo;
	}
	private set combo(value: number) {
		this._combo = value;
		this.emit('changeCombo', value);
	}

	private _score = 0;
	private get score() {
		return this._score;
	}
	private set score(value: number) {
		this._score = value;
		this.emit('changeScore', value);
	}

	private getMonoRenderOptions: null | ((mono: Mono) => Partial<Matter.IBodyRenderOptions>) = null;
	private getStoneRenderOptions: null | (() => Partial<Matter.IBodyRenderOptions>) = null;

	public replayPlaybackRate = 1;

	constructor(env: {
		seed: string;
		gameMode: DropAndFusionGame['gameMode'];
		getMonoRenderOptions?: (mono: Mono) => Partial<Matter.IBodyRenderOptions>;
		/**
		 * Enables the versus mode (mk-go, #3229). 1 人用では渡さない。
		 */
		versus?: boolean;
		getStoneRenderOptions?: () => Partial<Matter.IBodyRenderOptions>;
	}) {
		super();

		//#region BIND
		this.tick = this.tick.bind(this);
		//#endregion

		this.gameMode = env.gameMode;
		const parsed = parseGameMode(env.gameMode);
		if (parsed == null) throw new Error(`unknown game mode: ${env.gameMode}`);
		this.baseMode = parsed.base;
		this.physics = parsed.physics;
		this.getMonoRenderOptions = env.getMonoRenderOptions ?? null;
		this.getStoneRenderOptions = env.getStoneRenderOptions ?? null;
		this.rng = seedrandom(env.seed);
		this.garbageRng = seedrandom(`${env.seed}:garbage`);
		if (env.versus) {
			this.versus = { pending: 0, clearedCarry: 0 };
		}

		// sweetsモードは重いため
		const physicsQualityFactor = this.baseMode === 'sweets' ? 4 : this.PHYSICS_QUALITY_FACTOR;
		this.engine = Matter.Engine.create({
			constraintIterations: 2 * physicsQualityFactor,
			positionIterations: 6 * physicsQualityFactor,
			velocityIterations: 4 * physicsQualityFactor,
			gravity: {
				x: 0,
				y: this.baseMode === 'space' ? 0.0125 : 1,
			},
			timing: {
				timeScale: 2,
			},
			enableSleeping: false,
		});

		this.engine.world.bodies = [];

		//#region walls
		const WALL_OPTIONS: Matter.IChamferableBodyDefinition = {
			label: '_wall_',
			isStatic: true,
			// 静止した物体は matter.js が摩擦を 1 に書き換える (Body.setStatic) ので、この値は
			// 効いていない。玉との組は小さい方で決まるので、玉の摩擦がそのまま効く。
			friction: 0.7,
			slop: this.baseMode === 'space' ? 0.01 : 0.7,
			render: {
				strokeStyle: 'transparent',
				fillStyle: 'transparent',
			},
		};

		// mk-go (#3194): bouncy / space は左右の壁を上へ伸ばし (見えない)、下の角を塞ぐ。
		// はみ出しの判定に猶予がある (#3193) と、漂う玉 (space) は壁の上を越え、よく弾む
		// 玉 (bouncy) は挟まれて角から押し出され、どちらも箱の外へ出て消えたままゲームが
		// 続く。**既存のモードは変えない** — 壁の形が変わると、同じ記録でも結末が変わる
		// (版 4 のリプレイと途中保存が別の盤面になる)。内側の面は同じ。
		// 各対策を外すと流出が再発することは実測で確かめた (壁の厚さは効かなかったので
		// 本家のまま)。
		// **角も塞ぐ。** 本家の床は GAME_WIDTH の幅しか無く、左右の壁は床の上面の高さで
		// 終わっているので、壁の下・床の外 (左下 / 右下の角の外側) に隙間がある。挟まれた
		// 玉はそこから抜けていた (実測: (-94, 712) で消えた)。床を壁の外まで広げ、壁を
		// 床の下まで伸ばす。
		// #3216: BOUNCY はどの形でも当てる (弾む玉は形によらず挟まれて押し出される)。
		const containsEscapes = this.physics === 'bouncy' || this.baseMode === 'space';
		const thickness = this.WALL_THICKNESS;
		const wallTop = containsEscapes ? -this.CONTAINING_WALL_EXTRA_HEIGHT : 0;
		const wallBottom = containsEscapes ? this.GAME_HEIGHT + thickness : this.GAME_HEIGHT;
		// 床は壁の外側の面まで。はみ出させると、壁の外に玉が載る段差ができる。
		const floorWidth = containsEscapes ? this.GAME_WIDTH + ((thickness - this.PLAYAREA_MARGIN) * 2) : this.GAME_WIDTH;
		Matter.Composite.add(this.engine.world, [
			Matter.Bodies.rectangle(this.GAME_WIDTH / 2, this.GAME_HEIGHT + (thickness / 2) - this.PLAYAREA_MARGIN, floorWidth, thickness, WALL_OPTIONS),
			Matter.Bodies.rectangle(this.GAME_WIDTH + (thickness / 2) - this.PLAYAREA_MARGIN, (wallTop + wallBottom) / 2, thickness, wallBottom - wallTop, WALL_OPTIONS),
			Matter.Bodies.rectangle(-((thickness / 2) - this.PLAYAREA_MARGIN), (wallTop + wallBottom) / 2, thickness, wallBottom - wallTop, WALL_OPTIONS),
		]);
		//#endregion

		this.overflowCollider = Matter.Bodies.rectangle(this.GAME_WIDTH / 2, 0, this.GAME_WIDTH, 200, {
			label: '_overflow_',
			isStatic: true,
			isSensor: true,
			render: {
				strokeStyle: 'transparent',
				fillStyle: 'transparent',
			},
		});
		Matter.Composite.add(this.engine.world, this.overflowCollider);
	}

	public msToFrame(ms: number) {
		return Math.round(ms / this.TICK_DELTA);
	}

	public frameToMs(frame: number) {
		return frame * this.TICK_DELTA;
	}

	/**
	 * Body physics for the BOUNCY mode (mk-go, #3194).
	 *
	 * 重力は通常のまま、玉の性質だけを変える。**空気抵抗は通常のまま** — 0 にすると
	 * 玉がいつまでも止まらない。
	 */
	private static readonly BOUNCY_PHYSICS = {
		restitution: 0.9,
		friction: 0.01,
		frictionStatic: 0,
		// 速さの上限 (matter-js の velocity の単位。timeScale が 2 なので 1 tick に動く
		// 距離はこの約 2 倍)。よく弾む玉は挟まれると 60 を超える速さで押し出され、その
		// 勢いのまま壁の中を進んで抜ける。normal の最大は実測 13.2 (同じ単位) なので、
		// 普通の動きには効かない値にしてある。
		maxSpeed: 15,
		// #3216 で BOUNCY を他の形にも当てたときの上限。**NORMAL (= #3194 の bouncy) は
		// 15 のまま** — 変えると、これまでの記録とリプレイの結末が変わる。
		// 四角い玉は挟まれて床の下へ抜けやすく、15 だと 100 局中 1 局で抜けた (12 で 0)。
		maxSpeedShapes: 12,
		// sweets は重いので物理の精度を落としている (上の physicsQualityFactor)。その分
		// 速い玉が床をすり抜けやすく、15 のままだと 30 局中 3 局で玉が床の下へ抜けた。
		// 10 で 60 局中 0 (精度を 2 倍にしても 1 局残り、重さは 1.5 倍になった)。
		maxSpeedSweets: 10,
		// ほぼ止まっている玉に当たったときの跳ね返りを補う相手の速さの上限。物理エンジンは
		// 積み重なった玉への衝突で勢いを下の玉と床へ逃がすので、落ちている玉に当たっても
		// ほとんど跳ねない (実測: 同じ玉が床で 169px 跳ねるのに、玉の上では 1px に満たない。
		// 弾みを上げても玉の上は伸びない)。止まっている玉を床と同じ「動かない相手」とみなして補う。
		// 単位は maxSpeed と同じ (tick 後の velocity)。
		restingSpeed: 0.75,
		// 補う量の係数。目標は「近づいた速さ x 弾み x この係数」の離れる速さ。玉の上で床より
		// 少し高く跳ねるくらいを、実際に遊んで決めた (同じ玉で 1.0 なら 157px / 1.2 で 219px /
		// 1.4 で 283px、床は 169px)。
		assistRatio: 1.2,
	};

	/**
	 * Collisions of the BOUNCY mode with a (nearly) resting mono, to be given a
	 * proper rebound after the engine update (mk-go, #3194).
	 */
	private bounceAssists: { mover: Matter.Body; other: Matter.Body; nx: number; ny: number; approach: number }[] = [];

	/**
	 * Body physics for the FRICTION physics (mk-go, #3216).
	 *
	 * 玉どうしも壁も強く引っかかり、転がらずにその場で止まる。跳ねない。値は
	 * テストの計測で決めた (drop-and-fusion.test.ts の #3216)。
	 */
	/**
	 * The label of a garbage stone in the versus mode (mk-go, #3229).
	 */
	public static readonly STONE_LABEL = '_stone_';

	private static readonly FRICTION_PHYSICS = {
		restitution: 0,
		friction: 1,
		frictionStatic: 20,
		// 左右の壁に触れている玉の縦の速さと回転を、毎 tick この割合まで落とす
		// (壁にくっつく)。**止めきらない** — 玉は壁際ぎりぎりまで寄せて落とせるので、
		// 触れた瞬間に止めると落とした高さ (判定領域の中) に貼り付いて終わる。
		// ゆっくりずり落ちて最後は下へ届く。摩擦だけでは効かない — 垂直な壁は玉を
		// 横から押さないので、摩擦力が生まれない。
		wallGrip: 0.2,
		// 他の玉に触れている玉の速さ (縦横とも) を、毎 tick この割合まで落とす
		// (玉どうしもくっつく)。摩擦だけでは、落ちてきた玉が下の玉の上を滑り落ちる。
		monoGrip: 0.2,
	};

	private createBody(mono: Mono, x: number, y: number, stone = false) {
		const space = this.baseMode === 'space';
		const bouncy = this.physics === 'bouncy';
		const friction = this.physics === 'friction';
		const options = {
			label: stone ? DropAndFusionGame.STONE_LABEL : mono.id,
			density: space ? 0.01 : ((mono.sizeX * mono.sizeY) / 10000),
			restitution: space ? 0.5 : bouncy ? DropAndFusionGame.BOUNCY_PHYSICS.restitution : friction ? DropAndFusionGame.FRICTION_PHYSICS.restitution : 0.2,
			frictionAir: space ? 0 : 0.01,
			friction: space ? 0.5 : bouncy ? DropAndFusionGame.BOUNCY_PHYSICS.friction : friction ? DropAndFusionGame.FRICTION_PHYSICS.friction : 0.7,
			frictionStatic: space ? 0 : bouncy ? DropAndFusionGame.BOUNCY_PHYSICS.frictionStatic : friction ? DropAndFusionGame.FRICTION_PHYSICS.frictionStatic : 5,
			slop: space ? 0.01 : 0.7,
			//mass: 0,
			render: stone
				? (this.getStoneRenderOptions ? this.getStoneRenderOptions() : this.getMonoRenderOptions ? this.getMonoRenderOptions(mono) : undefined)
				: (this.getMonoRenderOptions ? this.getMonoRenderOptions(mono) : undefined),
		} satisfies Matter.IChamferableBodyDefinition;
		if (mono.shape === 'circle') {
			return Matter.Bodies.circle(x, y, mono.sizeX / 2, options);
		} else if (mono.shape === 'rectangle') {
			return Matter.Bodies.rectangle(x, y, mono.sizeX, mono.sizeY, options);
		} else if (mono.shape === 'custom' && mono.vertices != null && mono.verticesSize != null) { //eslint-disable-line @typescript-eslint/no-unnecessary-condition
			return Matter.Bodies.fromVertices(x, y, mono.vertices.map(i => i.map(j => ({
				x: (j.x / mono.verticesSize!) * mono.sizeX, //eslint-disable-line @typescript-eslint/no-non-null-assertion
				y: (j.y / mono.verticesSize!) * mono.sizeY, //eslint-disable-line @typescript-eslint/no-non-null-assertion
			}))), options);
		} else {
			throw new Error('unrecognized shape');
		}
	}

	private fusion(bodyA: Matter.Body, bodyB: Matter.Body) {
		if (this.latestFusionedAt > this.frame - this.COMBO_INTERVAL) {
			this.combo++;
		} else {
			this.combo = 1;
		}
		this.latestFusionedAt = this.frame;

		const newX = (bodyA.position.x + bodyB.position.x) / 2;
		const newY = (bodyA.position.y + bodyB.position.y) / 2;

		// **消す石は、合体した 2 つを消す前に決める** (消した後は位置が分からない)。
		// **対戦でなくても消す。** リプレイは対戦の指定なしで作られうるので、消し方が
		// 指定に依存すると、石は同じ位置に降るのに消えずに残って結末がずれる。1 人用は
		// 石が無いので何も起きない。
		const clearedStones = this.stonesTouching([bodyA, bodyB]);

		this.fusionReadyBodyIds = this.fusionReadyBodyIds.filter(x => x !== bodyA.id && x !== bodyB.id);
		this.gameOverReadyBodyIds = this.gameOverReadyBodyIds.filter(x => x !== bodyA.id && x !== bodyB.id);
		Matter.Composite.remove(this.engine.world, [bodyA, bodyB]);

		const currentMono = this.monoDefinitions.find(y => y.id === bodyA.label);

		if (currentMono == null) {
			throw new Error('Current Mono Not Found');
		}

		const nextMono = this.monoDefinitions.find(x => x.level === currentMono.level + 1) ?? null;

		if (nextMono) {
			const body = this.createBody(nextMono, newX, newY);
			Matter.Composite.add(this.engine.world, body);

			// 連鎖してfusionした場合の分かりやすさのため少し間を置いてからfusion対象になるようにする
			this.tickCallbackQueue.push({
				frame: this.frame + this.msToFrame(100),
				callback: () => {
					this.fusionReadyBodyIds.push(body.id);
				},
			});

			this.emit('monoAdded', nextMono);
		}

		this.clearStones(clearedStones);
		this.fusionAttack(clearedStones.length);

		const hasComboBonus = this.baseMode !== 'yen' && this.baseMode !== 'sweets';
		const comboBonus = hasComboBonus ? 1 + ((this.combo - 1) / 5) : 1;
		const additionalScore = Math.round(currentMono.score * comboBonus);
		this.score += additionalScore;

		this.emit('fusioned', newX, newY, nextMono, additionalScore);
	}

	private onCollision(event: Matter.IEventCollision<Matter.Engine>) {
		for (const pairs of event.pairs) {
			const { bodyA, bodyB } = pairs;

			// おじゃま石どうしは同じラベルでも合体しない (#3229)。
			const shouldFusion = (bodyA.label === bodyB.label) && bodyA.label !== DropAndFusionGame.STONE_LABEL &&
				!this.fusionReservedPairs.some(x =>
					x.bodyA.id === bodyA.id ||
					x.bodyA.id === bodyB.id ||
					x.bodyB.id === bodyA.id ||
					x.bodyB.id === bodyB.id);

			if (shouldFusion) {
				if (this.fusionReadyBodyIds.includes(bodyA.id) && this.fusionReadyBodyIds.includes(bodyB.id)) {
					this.fusion(bodyA, bodyB);
				} else {
					this.fusionReservedPairs.push({ bodyA, bodyB });
					this.tickCallbackQueue.push({
						frame: this.frame + this.msToFrame(100),
						callback: () => {
							this.fusionReservedPairs = this.fusionReservedPairs.filter(x => x.bodyA.id !== bodyA.id && x.bodyB.id !== bodyB.id);
							this.fusion(bodyA, bodyB);
						},
					});
				}
			} else {
				const energy = pairs.collision.depth;

				if (bodyA.label === '_overflow_' || bodyB.label === '_overflow_') continue;

				if (bodyA.label !== '_wall_' && bodyB.label !== '_wall_') {
					this.recordBounceAssist(bodyA.parent, bodyB.parent);
				}

				if (bodyA.label !== '_wall_' && bodyB.label !== '_wall_') {
					if (!this.gameOverReadyBodyIds.includes(bodyA.id)) this.gameOverReadyBodyIds.push(bodyA.id);
					if (!this.gameOverReadyBodyIds.includes(bodyB.id)) this.gameOverReadyBodyIds.push(bodyB.id);
				}

				this.emit('collision', energy, bodyA, bodyB);
			}
		}
	}

	private recordBounceAssist(a: Matter.Body, b: Matter.Body) {
		// BOUNCY だけ。**条件はここ 1 か所** — 適用 (tick) は物理を見ずに毎回呼ぶ。
		if (this.physics !== 'bouncy') return;
		// **単位を tick 後に揃える。** 衝突の通知は Engine.update の途中 (位置を進めた直後) に
		// 来るので、ここの velocity は 1 step の移動量で、tick 後の値 (applyBounceAssists と
		// limitSpeed が読む) の timeScale 倍になっている。揃えないと近づく速さを 2 倍に
		// 見積もり、跳ね返りが元の速さを上回る。
		const scale = 1 / this.engine.timing.timeScale;
		const va = { x: a.velocity.x * scale, y: a.velocity.y * scale };
		const vb = { x: b.velocity.x * scale, y: b.velocity.y * scale };
		const speedA = Math.sqrt((va.x * va.x) + (va.y * va.y));
		const speedB = Math.sqrt((vb.x * vb.x) + (vb.y * vb.y));
		const [mover, other, vm, vo] = speedA >= speedB ? [a, b, va, vb] : [b, a, vb, va];
		if (Math.min(speedA, speedB) > DropAndFusionGame.BOUNCY_PHYSICS.restingSpeed) return;
		const dx = other.position.x - mover.position.x;
		const dy = other.position.y - mover.position.y;
		const d = Math.sqrt((dx * dx) + (dy * dy));
		if (d === 0) return;
		const nx = dx / d;
		const ny = dy / d;
		// 衝突の瞬間 (まだ解決前) に近づいていた速さ。
		const approach = ((vm.x - vo.x) * nx) + ((vm.y - vo.y) * ny);
		if (approach <= 0) return;
		this.bounceAssists.push({ mover, other, nx, ny, approach });
	}

	/**
	 * Gives a mono that hit a resting mono about the rebound it would get from the
	 * floor (mk-go, #3194). 物理エンジンが解決した後に離れていく速さが、近づいた速さ x
	 * 弾みに届かなければ、その差を動いていた側に足す。記録順に処理するので決定的。
	 */
	private applyBounceAssists() {
		const assists = this.bounceAssists;
		if (assists.length === 0) return;
		this.bounceAssists = [];
		const alive = new Set(this.engine.world.bodies.map(b => b.id));
		for (const { mover, other, nx, ny, approach } of assists) {
			// 合体で消えた玉は対象外。
			if (!alive.has(mover.id) || !alive.has(other.id)) continue;
			// 動いていた側**自身**の離れる速さで見る (当てられた相手の動きに左右されない)。
			const separating = -((mover.velocity.x * nx) + (mover.velocity.y * ny));
			const wanted = approach * DropAndFusionGame.BOUNCY_PHYSICS.restitution * DropAndFusionGame.BOUNCY_PHYSICS.assistRatio;
			if (separating >= wanted) continue;
			const add = wanted - separating;
			Matter.Body.setVelocity(mover, {
				x: mover.velocity.x - (nx * add),
				y: mover.velocity.y - (ny * add),
			});
		}
	}

	/**
	 * Slows the monos touching a side wall so they cling to it and slide down
	 * slowly (mk-go, #3216, FRICTION).
	 */
	private gripWalls(grip: number): Set<Matter.Body['id']> {
		const gripped = new Set<Matter.Body['id']>();
		// **重なりではなく距離で見る。** 壁際に寄せて落とした玉は壁の内側の面にちょうど
		// 接するだけで重ならないので、衝突判定では「触れていない」ことになる。外形の端が
		// 面から 1px 以内なら触れているとみなす (外形は凸包の頂点から取るので、回転や
		// 非対称な形でも実際の端になる)。**触れていない玉に余裕を持たせない** — 回転した
		// お札や sweets は幅が縮むので、幅から余裕を計算すると空中の玉まで減速した
		// (#3216 の敵対的レビューで実測)。sweets は定義上の幅より細いので、壁際に寄せて
		// 落としても数 px 離れて触れないことがある。それは実際に触れていないので正しい。
		const left = this.PLAYAREA_MARGIN + 1;
		const right = this.GAME_WIDTH - this.PLAYAREA_MARGIN - 1;
		// world の並び順で処理するので決定的。
		for (const b of this.engine.world.bodies) {
			if (b.isStatic) continue;
			if (b.bounds.min.x > left && b.bounds.max.x < right) continue;
			Matter.Body.setVelocity(b, { x: b.velocity.x, y: b.velocity.y * grip });
			Matter.Body.setAngularVelocity(b, b.angularVelocity * grip);
			gripped.add(b.id);
		}
		return gripped;
	}

	/**
	 * Slows the monos touching another mono so they stick together (mk-go,
	 * #3216, FRICTION).
	 */
	private gripMonos(grip: number, onWall: Set<Matter.Body['id']>) {
		if (grip >= 1) return;
		const touching = new Set<Matter.Body['id']>();
		for (const pair of this.engine.pairs.list) {
			if (!pair.isActive || pair.isSensor) continue;
			const a = pair.bodyA.parent;
			const b = pair.bodyB.parent;
			if (a.isStatic || b.isStatic) continue;
			touching.add(a.id);
			touching.add(b.id);
		}
		// world の並び順で処理するので決定的。**壁でくっついた玉には重ねない** — 両方を
		// 掛けると 0.2 x 0.2 で縦の速さがほぼ 0 になり、壁際に支えの無い玉が宙づりの
		// 柱になって溜まる (レビューの実測で最大 9 個、判定領域にも掛かった)。
		for (const b of this.engine.world.bodies) {
			if (!touching.has(b.id) || onWall.has(b.id)) continue;
			Matter.Body.setVelocity(b, { x: b.velocity.x * grip, y: b.velocity.y * grip });
		}
	}

	private limitSpeed(maxSpeed: number) {
		for (const b of this.engine.world.bodies) {
			if (b.isStatic) continue;
			// Math.hypot は丸めがエンジンごとに違いうる (仕様が正確な丸めを要求しない)。
			// リプレイの結末がブラウザで変わらないよう、sqrt で計算する。
			const speed = Math.sqrt((b.velocity.x * b.velocity.x) + (b.velocity.y * b.velocity.y));
			if (speed > maxSpeed) {
				Matter.Body.setVelocity(b, {
					x: b.velocity.x * (maxSpeed / speed),
					y: b.velocity.y * (maxSpeed / speed),
				});
			}
		}
	}

	/**
	 * Ends the game when a mono has stayed in the overflow area for
	 * OVERFLOW_GRACE_MS (mk-go, #3193).
	 *
	 * 本家は判定領域に**1 フレームでも触れたら**終わる。合体で生まれた大きい玉が
	 * 周りの玉を押し広げると、玉が一瞬上へ弾かれることがあり、盤面に余裕があるのに
	 * 終わっていた。
	 *
	 * **毎フレーム重なりを調べる** (衝突イベントの発火順に頼らない)。合体して
	 * 消えた玉は world から外れているので、ここで自然に記録から落ちる。
	 * **フレーム数で数える** — 実時間を使うとリプレイと途中保存の早送りが別の
	 * 結果になる。
	 */
	private checkOverflow() {
		// 他の玉とぶつかったことのある玉だけを数える (落とした直後の玉が上部を
		// 通過しても終わらない)。複数の部品でできた玉 (sweets) は、部品の id で
		// 記録されていることがあるので、部品まで見る。
		const candidates = this.engine.world.bodies.filter(b =>
			b.id !== this.overflowCollider.id &&
			b.parts.some(p => this.gameOverReadyBodyIds.includes(p.id)));

		const hits = new Set<Matter.Body['id']>();
		for (const collision of Matter.Query.collides(this.overflowCollider, candidates)) {
			const other = collision.bodyA.parent.id === this.overflowCollider.id ? collision.bodyB : collision.bodyA;
			hits.add(other.parent.id);
		}

		// 念のため: 壁や床を丸ごと突き抜けて失われた玉があれば終了する。本家は判定領域に
		// 触れた瞬間に終わるので、箱の外へ出た玉がそのまま残ることは無かった。猶予を
		// 持たせた今、起きると玉が消えたまま続き、実質「玉を捨てる」操作になる (よく弾む
		// 物理で実際に起きた。今のモードでは実測 0)。**壁の内側の面では判定しない** —
		// 挟まれた玉は一瞬めり込んで次の tick で押し戻される。そこで終わらせると、それ
		// 自体が理不尽なゲームオーバーになる。
		for (const b of this.engine.world.bodies) {
			if (b.isStatic) continue;
			if (b.position.x < -this.WALL_THICKNESS || b.position.x > this.GAME_WIDTH + this.WALL_THICKNESS || b.position.y > this.GAME_HEIGHT + this.WALL_THICKNESS) {
				this.gameOver();
				return;
			}
		}

		for (const id of [...this.overflowingSince.keys()]) {
			if (!hits.has(id)) this.overflowingSince.delete(id);
		}
		for (const id of hits) {
			if (!this.overflowingSince.has(id)) this.overflowingSince.set(id, this.frame);
		}

		const overflowing = this.overflowingSince.size > 0;
		if (overflowing !== this.overflowing) {
			this.overflowing = overflowing;
			this.emit('overflowWarning', overflowing);
		}

		const grace = this.msToFrame(this.OVERFLOW_GRACE_MS);
		for (const since of this.overflowingSince.values()) {
			if (this.frame - since >= grace) {
				this.gameOver();
				return;
			}
		}
	}

	public surrender() {
		this.logs.push({
			frame: this.frame,
			operation: 'surrender',
		});

		this.gameOver();
	}

	private gameOver() {
		if (this.isGameOver) return;
		this.isGameOver = true;
		// **gameOver を先に出す。** 警告の解除を先に出すと、受け取る側からは
		// 「はみ出しが解けて続いた」ように見える。
		this.emit('gameOver');
		if (this.overflowing) {
			this.overflowing = false;
			this.emit('overflowWarning', false);
		}
	}

	public start() {
		for (let i = 0; i < this.STOCK_MAX; i++) {
			this.stock.push({
				id: this.rng().toString(),
				mono: this.monoDefinitions.filter(x => x.dropCandidate)[Math.floor(this.rng() * this.monoDefinitions.filter(x => x.dropCandidate).length)],
			});
		}
		this.emit('changeStock', this.stock);

		Matter.Events.on(this.engine, 'collisionStart', this.onCollision.bind(this));
	}

	public getLogs() {
		return this.logs;
	}

	public tick() {
		this.frame++;

		if (this.latestFusionedAt < this.frame - this.COMBO_INTERVAL) {
			this.combo = 0;
		}

		this.tickCallbackQueue = this.tickCallbackQueue.filter(x => {
			if (x.frame === this.frame) {
				x.callback();
				return false;
			} else {
				return true;
			}
		});

		Matter.Engine.update(this.engine, this.TICK_DELTA);

		// 記録は BOUNCY のときだけ (recordBounceAssist)。ほかの物理では空なので何も起きない。
		this.applyBounceAssists();
		if (this.physics === 'bouncy') {
			const p = DropAndFusionGame.BOUNCY_PHYSICS;
			this.limitSpeed(this.baseMode === 'normal' ? p.maxSpeed : this.baseMode === 'sweets' ? p.maxSpeedSweets : p.maxSpeedShapes);
		}
		if (this.physics === 'friction') {
			const onWall = this.gripWalls(DropAndFusionGame.FRICTION_PHYSICS.wallGrip);
			this.gripMonos(DropAndFusionGame.FRICTION_PHYSICS.monoGrip, onWall);
		}

		if (!this.isGameOver) this.checkOverflow();

		const hasNextTick = !this.isGameOver;

		return hasNextTick;
	}

	public getActiveMonos() {
		return this.engine.world.bodies
			.map(x => this.monoDefinitions.find((mono) => mono.id === x.label))
			.filter(x => x !== undefined);
	}

	public drop(_x: number) {
		if (this.isGameOver) return;
		if (this.frame - this.latestDroppedAt < this.DROP_COOLTIME) return;

		const head = this.stock.shift();
		if (!head) return;

		this.stock.push({
			id: this.rng().toString(),
			mono: this.monoDefinitions.filter(x => x.dropCandidate)[Math.floor(this.rng() * this.monoDefinitions.filter(x => x.dropCandidate).length)],
		});
		this.emit('changeStock', this.stock);

		const inputX = Math.round(_x);
		const x = Math.min(this.GAME_WIDTH - this.PLAYAREA_MARGIN - (head.mono.sizeX / 2), Math.max(this.PLAYAREA_MARGIN + (head.mono.sizeX / 2), inputX));
		const body = this.createBody(head.mono, x, 50 + head.mono.sizeY / 2);
		this.logs.push({
			frame: this.frame,
			operation: 'drop',
			x: inputX,
		});

		// add force
		if (this.baseMode === 'space') {
			Matter.Body.applyForce(body, body.position, {
				x: 0,
				y: (Math.PI * head.mono.sizeX * head.mono.sizeY) / 65536,
			});
		}

		Matter.Composite.add(this.engine.world, body);

		this.fusionReadyBodyIds.push(body.id);
		this.latestDroppedAt = this.frame;

		this.emit('dropped', x);
		this.emit('monoAdded', head.mono);

		// 届いている石は、受け手が玉を落とした後に降る (#3229)。**リプレイでは
		// 何もしない** — リプレイは receiveAttack を呼ばないので pending が 0 で、石は
		// 記録の garbage を当てて降らせる。
		this.releaseGarbage();
	}

	public hold() {
		if (this.isGameOver) return;

		this.logs.push({
			frame: this.frame,
			operation: 'hold',
		});

		if (this.holding) {
			const head = this.stock.shift();
			if (!head) return;
			this.stock.unshift(this.holding);
			this.holding = head;
			this.emit('changeHolding', this.holding);
			this.emit('changeStock', this.stock);
		} else {
			const head = this.stock.shift();
			if (!head) return;
			this.holding = head;
			this.stock.push({
				id: this.rng().toString(),
				mono: this.monoDefinitions.filter(x => x.dropCandidate)[Math.floor(this.rng() * this.monoDefinitions.filter(x => x.dropCandidate).length)],
			});
			this.emit('changeHolding', this.holding);
			this.emit('changeStock', this.stock);
		}
	}

	public static serializeLogs(logs: Log[]) {
		const _logs: number[][] = [];

		for (let i = 0; i < logs.length; i++) {
			const log = logs[i];
			const frameDelta = i === 0 ? log.frame : log.frame - logs[i - 1].frame;

			switch (log.operation) {
				case 'drop':
					_logs.push([frameDelta, 0, log.x]);
					break;
				case 'hold':
					_logs.push([frameDelta, 1]);
					break;
				case 'surrender':
					_logs.push([frameDelta, 2]);
					break;
				case 'garbage':
					_logs.push([frameDelta, 3, log.count]);
					break;
			}
		}

		return _logs;
	}

	public static deserializeLogs(logs: number[][]) {
		const _logs: Log[] = [];

		let frame = 0;

		for (const log of logs) {
			const frameDelta = log[0];
			frame += frameDelta;

			const operation = log[1];

			switch (operation) {
				case 0:
					_logs.push({
						frame,
						operation: 'drop',
						x: log[2],
					});
					break;
				case 1:
					_logs.push({
						frame,
						operation: 'hold',
					});
					break;
				case 2:
					_logs.push({
						frame,
						operation: 'surrender',
					});
					break;
				case 3:
					_logs.push({
						frame,
						operation: 'garbage',
						count: log[2],
					});
					break;
			}
		}

		return _logs;
	}

	/**
	 * Applies one recorded operation (mk-go, #3229). リプレイと途中保存の早送りは
	 * これを通す — 種類ごとの分岐を呼び出し側に書くと、種類を足したときに取りこぼす
	 * (drop / hold 以外を surrender として扱う形が実際にあった)。
	 */
	public applyLog(log: Log) {
		switch (log.operation) {
			case 'drop': this.drop(log.x); break;
			case 'hold': this.hold(); break;
			case 'surrender': this.surrender(); break;
			case 'garbage': this.dropGarbage(log.count); break;
		}
	}

	/**
	 * Queues stones sent by the opponent (versus mode, mk-go #3229). They fall
	 * after this board's next drop.
	 */
	public receiveAttack(count: number) {
		// **相手から届く値なので検査する。** NaN が入ると予告が NaN のまま戻らず、以後の
		// 攻撃も全部消える。Infinity は毎手番 5 個を永遠に降らせる。
		if (!this.versus || this.isGameOver || !Number.isFinite(count) || count <= 0) return;
		this.versus.pending = Math.min(VERSUS_RULES.maxPendingGarbage, this.versus.pending + Math.floor(count));
		this.emit('changePendingGarbage', this.versus.pending);
	}

	/**
	 * The number of stones waiting to fall (versus mode).
	 */
	public get pendingGarbage() {
		return this.versus?.pending ?? 0;
	}

	private releaseGarbage() {
		if (!this.versus || this.versus.pending <= 0) return;
		const count = Math.min(this.versus.pending, VERSUS_RULES.maxGarbagePerDrop);
		this.versus.pending -= count;
		this.emit('changePendingGarbage', this.versus.pending);
		this.dropGarbage(count);
	}

	/**
	 * Drops stones on this board and records it. 位置は石専用の乱数で決める。
	 */
	private dropGarbage(_count: number) {
		if (this.isGameOver || !Number.isFinite(_count) || _count <= 0) return;
		// 記録から来る数も上限で頭打ちにする (実際の対戦では超えない)。壊れた途中保存に
		// 大きな数が入っていると、玉を何百万個も作ってタブが固まる。
		const count = Math.min(Math.floor(_count), VERSUS_RULES.maxGarbagePerDrop);
		this.logs.push({ frame: this.frame, operation: 'garbage', count });

		const lv = this.monoDefinitions.find(x => x.level === VERSUS_RULES.stoneLevel);
		if (lv == null) throw new Error('stone mono not found');
		// **石は形によらず円にする** (SQUARE の Lv2 は四角、SWEETS は多角形)。触れて
		// いるかの判定 (distanceBetween) が円を前提にしている。
		const stoneMono: Mono = { ...lv, shape: 'circle', sizeY: lv.sizeX };
		// 横に等分した枠へ 1 個ずつ置く (同じ場所に重ねて出すと、生まれた瞬間に
		// 弾け飛ぶ)。枠の順番を石の乱数で並べ替える。
		const left = this.PLAYAREA_MARGIN + (stoneMono.sizeX / 2);
		const right = this.GAME_WIDTH - this.PLAYAREA_MARGIN - (stoneMono.sizeX / 2);
		const slots = Math.max(1, Math.floor((right - left) / stoneMono.sizeX) + 1);
		const order = [...Array(slots).keys()];
		for (let i = order.length - 1; i > 0; i--) {
			const j = Math.floor(this.garbageRng() * (i + 1));
			[order[i], order[j]] = [order[j], order[i]];
		}
		for (let i = 0; i < count; i++) {
			const slot = order[i % slots];
			const row = Math.floor(i / slots);
			const x = slots === 1 ? (left + right) / 2 : left + ((right - left) * slot / (slots - 1));
			// **落とした玉 (上端が y=50) より上に出す。** 同じ高さに出すと、今落とした玉と
			// 重なって弾き、狙った位置をずらす (#3229 のレビューで実測: 約 18% の石が重なった)。
			const y = 50 - (stoneMono.sizeY / 2) - 1 - (row * stoneMono.sizeY);
			const body = this.createBody(stoneMono, x, y, true);
			// SPACE は重力が弱いので、玉と同じく下向きの力を与える。与えないと石が
			// はみ出しの判定領域に居続けて終わる (実測: 30 シード中 3 回)。
			if (this.baseMode === 'space') {
				Matter.Body.applyForce(body, body.position, {
					x: 0,
					y: (Math.PI * stoneMono.sizeX * stoneMono.sizeY) / 65536,
				});
			}
			Matter.Composite.add(this.engine.world, body);
		}
	}

	/**
	 * Stones touching any of the bodies (versus mode).
	 */
	private stonesTouching(bodies: Matter.Body[]): Matter.Body[] {
		const stones = this.engine.world.bodies.filter(b => b.label === DropAndFusionGame.STONE_LABEL);
		if (stones.length === 0) return [];
		return stones.filter(stone => bodies.some(b => this.distanceBetween(stone, b) <= VERSUS_RULES.touchMargin));
	}

	/**
	 * The gap between two bodies (0 when they overlap). 石は円なので、石の中心から
	 * 相手の輪郭までの距離から半径を引く。
	 */
	private distanceBetween(stone: Matter.Body, other: Matter.Body): number {
		if (Matter.Collision.collides(stone, other) != null) return 0;
		const r = stone.circleRadius ?? ((stone.bounds.max.x - stone.bounds.min.x) / 2);
		const c = stone.position;
		let min = Infinity;
		const parts = other.parts.length > 1 ? other.parts.slice(1) : other.parts;
		for (const part of parts) {
			const vs = part.vertices;
			for (let i = 0; i < vs.length; i++) {
				const a = vs[i];
				const b = vs[(i + 1) % vs.length];
				const dx = b.x - a.x;
				const dy = b.y - a.y;
				const len2 = (dx * dx) + (dy * dy);
				const t = len2 === 0 ? 0 : Math.max(0, Math.min(1, (((c.x - a.x) * dx) + ((c.y - a.y) * dy)) / len2));
				const px = a.x + (t * dx) - c.x;
				const py = a.y + (t * dy) - c.y;
				min = Math.min(min, Math.sqrt((px * px) + (py * py)));
			}
		}
		return Math.max(0, min - r);
	}

	/**
	 * Removes stones cleared by a fusion (versus mode).
	 */
	private clearStones(stones: Matter.Body[]) {
		if (stones.length === 0) return;
		Matter.Composite.remove(this.engine.world, stones);
		const ids = new Set(stones.map(b => b.id));
		this.gameOverReadyBodyIds = this.gameOverReadyBodyIds.filter(x => !ids.has(x));
		this.emit('stonesCleared', stones.length);
	}

	/**
	 * Sends the attack of one fusion (versus mode).
	 *
	 * 攻撃はコンボ (2 コンボ目から 1 コンボごとに +1) + 消した石 (stonesPerAttack 個で
	 * 1 個、端数は持ち越す)。**合体した玉の大きさは数えない** — 数えると合体のたびに
	 * 送ることになり、試算で 1 人あたり毎分 100 個を超えて相手がすぐ埋まった (#3231)。
	 */
	private fusionAttack(clearedCount: number) {
		if (!this.versus) return;
		this.versus.clearedCarry += clearedCount;
		const fromStones = Math.floor(this.versus.clearedCarry / VERSUS_RULES.stonesPerAttack);
		this.versus.clearedCarry %= VERSUS_RULES.stonesPerAttack;
		const attack = Math.max(0, this.combo - 1) + fromStones;
		if (attack > 0) this.emit('attack', attack);
	}

	public dispose() {
		Matter.World.clear(this.engine.world, false);
		Matter.Engine.clear(this.engine);
	}
}
