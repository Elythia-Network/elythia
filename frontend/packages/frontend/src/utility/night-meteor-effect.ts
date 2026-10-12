/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Elythia: draws a few meteors on the screen now and then at night (#3530).
 * Loaded lazily from `boot/main-boot.ts` only while the preference is on.
 */

import { nightMeteorCheckDelay, shouldShowNightMeteor } from '@/utility/night-meteor.js';

/** How the meteors look. The values were chosen on the prototype in #3530. */
export const NIGHT_METEOR_STYLE = {
	/** The average wait between the bursts. */
	gapMs: 25 * 1000,
	/** The wait is the average multiplied by a random factor in this range. */
	gapJitter: [0.6, 1.4],
	/** How many meteors fly in one burst. */
	burst: 3,
	/** The angle from the horizontal. They fly from the upper right to the lower left. */
	angleDeg: 70,
	/** Where they appear, as the fractions of the screen width. */
	xRange: [0, 1],
	/** The multiplier for the base speed (380 to 520 px/s). */
	speed: 1.6,
	/** The length of the tail in CSS pixels. */
	tail: 140,
	/** The width of the line in CSS pixels. */
	width: 1.5,
	/** The opacity at the brightest. */
	brightness: 0.9,
	/** The warm white of the stars on the splash screen. */
	color: '#fff6d8',
	/** The same color, fully transparent, for the end of the tail. */
	tailEndColor: '#fff6d800',
} as const;

type Meteor = {
	x: number;
	y: number;
	vx: number;
	vy: number;
	/** Seconds since it appeared. Negative while it waits for its turn in the burst. */
	life: number;
	/** Seconds until it fades out. */
	max: number;
};

/** Returns the wait before the next burst. */
export function nextBurstDelay(random: () => number = Math.random): number {
	const [lo, hi] = NIGHT_METEOR_STYLE.gapJitter;
	return NIGHT_METEOR_STYLE.gapMs * (lo + random() * (hi - lo));
}

/**
 * Draws the meteors on a canvas over the whole screen.
 * The canvas is in the document and requestAnimationFrame runs only while a
 * meteor is in flight. Between the bursts only a timer is left.
 */
export class NightMeteorRenderer {
	private active = false;
	private canvas: HTMLCanvasElement | null = null;
	private ctx: CanvasRenderingContext2D | null = null;
	private meteors: Meteor[] = [];
	private raf = 0;
	private lastFrame = 0;
	private burstTimer: number | null = null;
	private width = 0;
	private height = 0;
	private dpr = 1;

	constructor() {
		this.frame = this.frame.bind(this);
		this.onResize = this.onResize.bind(this);
	}

	/** Whether the bursts are scheduled (between `start()` and `stop()`). */
	public get running(): boolean {
		return this.active;
	}

	public start(): void {
		if (this.active) return;
		this.active = true;
		// 始めてすぐに流すと、タブを行き来するたびに流れて目障りなので、最初も間を空ける。
		this.scheduleBurst();
	}

	public stop(): void {
		if (!this.active) return;
		this.active = false;
		if (this.burstTimer != null) window.clearTimeout(this.burstTimer);
		this.burstTimer = null;
		this.detach();
	}

	/**
	 * Puts the canvas in the document. Returns false when the 2D canvas is unavailable.
	 *
	 * 画面全体の canvas を置いたままにすると、何も描いていない間も合成のレイヤーと
	 * 描画先のメモリ(1920x1080、倍率2で約33MB)を持ち続ける。流れている間だけ置く。
	 */
	private attach(): boolean {
		if (this.canvas != null) return true;

		const canvas = window.document.createElement('canvas');
		const ctx = canvas.getContext('2d');
		// 2D の canvas が使えない環境では、何も出さないだけにする。
		if (ctx == null) return false;

		Object.assign(canvas.style, {
			position: 'fixed',
			// 100vh はスマートフォンでアドレスバーの分だけ見えている範囲とずれるので、四辺を 0 にして画面に合わせる。
			top: '0',
			right: '0',
			bottom: '0',
			left: '0',
			width: '100%',
			height: '100%',
			background: 'transparent',
			pointerEvents: 'none',
			// 季節の演出の canvas と同じく、全ての要素の上に重ねる(押す操作は pointer-events で素通しする)。
			zIndex: '2147483647',
		});
		canvas.setAttribute('aria-hidden', 'true');
		window.document.body.append(canvas);

		this.canvas = canvas;
		this.ctx = ctx;
		this.resize(true);
		window.addEventListener('resize', this.onResize);
		return true;
	}

	private detach(): void {
		if (this.raf !== 0) window.cancelAnimationFrame(this.raf);
		this.raf = 0;
		this.lastFrame = 0;
		this.meteors = [];
		if (this.canvas == null) return;
		window.removeEventListener('resize', this.onResize);
		// 外しただけでは描画先のメモリが GC まで残り、iOS Safari では canvas の総量の上限に数えられる。大きさを 0 にしてその場で手放す。
		this.canvas.width = 0;
		this.canvas.height = 0;
		this.canvas.remove();
		this.canvas = null;
		this.ctx = null;
	}

	private onResize(): void {
		this.resize(false);
	}

	private resize(force: boolean): void {
		if (this.canvas == null || this.ctx == null) return;
		const dpr = Math.min(window.devicePixelRatio || 1, 2);
		// 描画先の縦横比を、実際に表示されている canvas の大きさに合わせる。
		const width = this.canvas.clientWidth || window.innerWidth;
		const height = this.canvas.clientHeight || window.innerHeight;
		if (!force && width === this.width && height === this.height && dpr === this.dpr) return;
		this.width = width;
		this.height = height;
		this.dpr = dpr;
		// canvas の大きさを変えると中身と変換が消えるので、変換は毎回入れ直す。
		this.canvas.width = Math.round(width * dpr);
		this.canvas.height = Math.round(height * dpr);
		this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
	}

	private scheduleBurst(): void {
		this.burstTimer = window.setTimeout(() => {
			this.burstTimer = null;
			this.launch();
			this.scheduleBurst();
		}, nextBurstDelay());
	}

	private launch(): void {
		if (!this.attach()) return;

		const s = NIGHT_METEOR_STYLE;
		const [xLo, xHi] = s.xRange;
		for (let k = 0; k < s.burst; k++) {
			const angle = (s.angleDeg + (Math.random() - 0.5) * 6) * Math.PI / 180;
			const v = (380 + Math.random() * 140) * s.speed;
			this.meteors.push({
				x: this.width * (xLo + Math.random() * (xHi - xLo)),
				y: -10 - k * 18 - Math.random() * 30,
				vx: -Math.cos(angle) * v,
				vy: Math.sin(angle) * v,
				life: -k * (0.12 + Math.random() * 0.25),
				max: 0.9 + Math.random() * 0.5,
			});
		}

		if (this.raf === 0) {
			this.lastFrame = 0;
			this.raf = window.requestAnimationFrame(this.frame);
		}
	}

	private frame(now: number): void {
		this.raf = 0;
		const ctx = this.ctx;
		if (ctx == null) return;

		const dt = Math.min(0.05, (now - (this.lastFrame || now)) / 1000);
		this.lastFrame = now;
		ctx.clearRect(0, 0, this.width, this.height);

		const s = NIGHT_METEOR_STYLE;
		for (const m of this.meteors) {
			m.life += dt;
			if (m.life < 0) continue;
			m.x += m.vx * dt;
			m.y += m.vy * dt;
			const t = m.life / m.max;
			const alpha = s.brightness * Math.min(1, t * 6) * Math.max(0, 1 - t);
			const speed = Math.hypot(m.vx, m.vy);
			const tx = m.x - (m.vx / speed) * s.tail;
			const ty = m.y - (m.vy / speed) * s.tail;
			const gradient = ctx.createLinearGradient(m.x, m.y, tx, ty);
			gradient.addColorStop(0, s.color);
			// 'transparent' は透明な黒なので、途中が灰色がかる。同じ色のまま透明にする。
			gradient.addColorStop(1, s.tailEndColor);
			ctx.globalAlpha = alpha;
			ctx.strokeStyle = gradient;
			ctx.lineWidth = s.width;
			ctx.lineCap = 'round';
			ctx.beginPath();
			ctx.moveTo(m.x, m.y);
			ctx.lineTo(tx, ty);
			ctx.stroke();
			ctx.fillStyle = s.color;
			ctx.beginPath();
			ctx.arc(m.x, m.y, s.width * 0.9, 0, Math.PI * 2);
			ctx.fill();
		}
		ctx.globalAlpha = 1;
		this.meteors = this.meteors.filter(m => m.life < m.max);

		// 流れ星が残っていないときは次の群れまで描かない。requestAnimationFrame を回し続けず、canvas も外す。
		if (this.meteors.length === 0) {
			this.detach();
			return;
		}
		this.raf = window.requestAnimationFrame(this.frame);
	}
}

/** The state outside this module that the meteors depend on. */
export type NightMeteorEnvironment = {
	/** Whether the applied theme is a dark one (`data-color-scheme` on `<html>`). */
	darkMode: boolean;
	/** The in-app animation preference (`prefer.s.animation`). */
	animation: boolean;
	/** Whether the seasonal screen effect was started at boot. */
	seasonalEffectShowing: boolean;
};

/** Controls the meteors while the preference is on. */
export type NightMeteorController = {
	/** Checks the conditions again. Call it when the environment changes. */
	update(): void;
	/** Stops the meteors and removes every listener and timer. */
	stop(): void;
	/** Whether the conditions hold and the bursts are scheduled. */
	readonly showing: boolean;
};

const REDUCED_MOTION_QUERY = '(prefers-reduced-motion: reduce)';

/**
 * Starts watching the conditions and shows the meteors while they hold.
 * The caller calls `update()` when the environment changes (the theme or the
 * animation preference) and `stop()` when the preference is turned off.
 */
export function startNightMeteorEffect(getEnvironment: () => NightMeteorEnvironment): NightMeteorController {
	const renderer = new NightMeteorRenderer();
	const reducedMotion = window.matchMedia(REDUCED_MOTION_QUERY);
	let checkTimer: number | null = null;
	let stopped = false;

	function update(): void {
		if (stopped) return;
		const now = new Date();
		const env = getEnvironment();
		const show = shouldShowNightMeteor({
			enabled: true,
			darkMode: env.darkMode,
			animation: env.animation,
			reducedMotion: reducedMotion.matches,
			visible: window.document.visibilityState === 'visible',
			seasonalEffectShowing: env.seasonalEffectShowing,
			now,
		});
		if (show) {
			renderer.start();
		} else {
			renderer.stop();
		}

		// 毎フレーム時刻を見る代わりに、夜の始まりか終わりの時刻にだけ見直す。
		if (checkTimer != null) window.clearTimeout(checkTimer);
		checkTimer = window.setTimeout(update, nightMeteorCheckDelay(now));
	}

	window.document.addEventListener('visibilitychange', update);
	reducedMotion.addEventListener('change', update);
	update();

	return {
		update,
		get showing() {
			return renderer.running;
		},
		stop() {
			stopped = true;
			if (checkTimer != null) window.clearTimeout(checkTimer);
			checkTimer = null;
			window.document.removeEventListener('visibilitychange', update);
			reducedMotion.removeEventListener('change', update);
			renderer.stop();
		},
	};
}
