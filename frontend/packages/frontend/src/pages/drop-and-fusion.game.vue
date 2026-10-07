<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div class="_spacer" style="--MI_SPACER-w: 800px;">
	<div :class="$style.root">
		<div v-if="!gameLoaded" :class="$style.loadingScreen">
			<!-- mk-go (#3192): 途中保存からの早送り中は進捗を出す。長いゲームだと数秒かかる。 -->
			<div v-if="fastForwardProgress != null">{{ i18n.ts._mkgoBubbleGame.resuming }} {{ Math.floor(fastForwardProgress * 100) }}%</div>
			<div v-else>{{ i18n.ts.loading }}<MkEllipsis/></div>
		</div>
		<!-- ↓に対してTransitionコンポーネントを使うと何故かkeyを指定していてもキャッシュが効かず様々なコンポーネントが都度再評価されてパフォーマンスが低下する -->
		<div v-show="gameLoaded" class="_gaps_s">
			<div v-if="readyGo === 'ready'" :class="$style.readyGo_bg">
			</div>
			<Transition
				:enterActiveClass="$style.transition_zoom_enterActive"
				:leaveActiveClass="$style.transition_zoom_leaveActive"
				:enterFromClass="$style.transition_zoom_enterFrom"
				:leaveToClass="$style.transition_zoom_leaveTo"
				:moveClass="$style.transition_zoom_move"
				mode="default"
			>
				<div v-if="readyGo === 'ready'" :class="$style.readyGo_ready">
					<img src="/client-assets/drop-and-fusion/ready.png" :class="$style.readyGo_img"/>
				</div>
				<div v-else-if="readyGo === 'go'" :class="$style.readyGo_go">
					<img src="/client-assets/drop-and-fusion/go.png" :class="$style.readyGo_img"/>
				</div>
			</Transition>

			<div :class="$style.header">
				<div class="_woodenFrame" :class="[$style.headerTitle]">
					<div class="_woodenFrameInner">
						<b>{{ i18n.ts.bubbleGame }}</b>
						<div>- {{ dropAndFusionModeLabel(gameMode) }} -</div>
					</div>
				</div>
				<div class="_woodenFrame _woodenFrameH">
					<div class="_woodenFrameInner">
						<MkButton inline small @click="hold">{{ i18n.ts._bubbleGame.hold }}</MkButton>
						<img v-if="holdingStock" :src="getTextureImageUrl(holdingStock.mono)" style="width: 32px; margin-left: 8px; vertical-align: bottom;"/>
					</div>
					<div class="_woodenFrameInner" :class="$style.stock" style="text-align: center;">
						<TransitionGroup
							:enterActiveClass="$style.transition_stock_enterActive"
							:leaveActiveClass="$style.transition_stock_leaveActive"
							:enterFromClass="$style.transition_stock_enterFrom"
							:leaveToClass="$style.transition_stock_leaveTo"
							:moveClass="$style.transition_stock_move"
						>
							<img v-for="x in stock" :key="x.id" :src="getTextureImageUrl(x.mono)" style="width: 32px; vertical-align: bottom;"/>
						</TransitionGroup>
					</div>
				</div>
			</div>

			<div ref="containerEl" :class="[$style.gameContainer, { [$style.gameOver]: isGameOver && !replaying }]" @contextmenu.stop.prevent @click.stop.prevent="onClick" @touchmove.stop.prevent="onTouchmove" @touchend="onTouchend" @mousemove="onMousemove">
				<img v-if="store.s.darkMode" src="/client-assets/drop-and-fusion/frame-dark.svg" :class="$style.mainFrameImg"/>
				<img v-else src="/client-assets/drop-and-fusion/frame-light.svg" :class="$style.mainFrameImg"/>
				<canvas ref="canvasEl" :class="$style.canvas"></canvas>
				<!--
					mk-go (#3193): はみ出している間、判定領域の下端を点滅させる。判定は
					「判定領域に 2 秒とどまったら終了」なので、猶予があることと、このままだと
					終わることを伝える。
				-->
				<div v-if="overflowWarning && !isGameOver" :class="$style.overflowWarning"></div>
				<Transition
					:enterActiveClass="$style.transition_combo_enterActive"
					:leaveActiveClass="$style.transition_combo_leaveActive"
					:enterFromClass="$style.transition_combo_enterFrom"
					:leaveToClass="$style.transition_combo_leaveTo"
					:moveClass="$style.transition_combo_move"
				>
					<div v-show="combo > 1" :class="$style.combo" :style="{ fontSize: `${100 + ((comboPrev - 2) * 15)}%` }">{{ comboPrev }} Chain!</div>
				</Transition>
				<div v-if="!isGameOver && !replaying && readyGo !== 'ready'" :class="$style.dropperContainer" :style="{ left: dropperX + 'px' }">
					<!--<img v-if="currentPick" src="/client-assets/drop-and-fusion/dropper.png" :class="$style.dropper" :style="{ left: dropperX + 'px' }"/>-->
					<Transition
						:enterActiveClass="$style.transition_picked_enterActive"
						:leaveActiveClass="$style.transition_picked_leaveActive"
						:enterFromClass="$style.transition_picked_enterFrom"
						:leaveToClass="$style.transition_picked_leaveTo"
						:moveClass="$style.transition_picked_move"
						mode="out-in"
					>
						<img v-if="currentPick" :key="currentPick.id" :src="getTextureImageUrl(currentPick.mono)" :class="$style.currentMono" :style="{ marginBottom: -((currentPick?.mono.sizeY * viewScale) / 2) + 'px', left: -((currentPick?.mono.sizeX * viewScale) / 2) + 'px', width: `${currentPick?.mono.sizeX * viewScale}px` }"/>
					</Transition>
					<template v-if="dropReady && currentPick">
						<img src="/client-assets/drop-and-fusion/drop-arrow.svg" :class="$style.currentMonoArrow"/>
						<div :class="$style.dropGuide"></div>
					</template>
				</div>
				<div v-if="isGameOver && !replaying" :class="$style.gameOverLabel">
					<div class="_gaps_s">
						<div v-if="timeUp" :class="$style.timeUpLabel">{{ i18n.ts._mkgoBubbleGame._versus.timeUp }}</div>
						<img v-else src="/client-assets/drop-and-fusion/gameover.png" style="width: 200px; max-width: 100%; display: block; margin: auto; margin-bottom: -5px;"/>
						<div>{{ i18n.ts._bubbleGame._score.score }}: <MkNumber :value="score"/>{{ dropAndFusionScoreUnit(gameMode) }}</div>
						<div>{{ i18n.ts._bubbleGame._score.maxChain }}: <MkNumber :value="maxCombo"/></div>
						<div v-if="baseMode === 'yen'">
							{{ i18n.ts._bubbleGame._score.scoreYen }}:
							<I18n :src="i18n.ts._bubbleGame._score.yen" tag="b">
								<template #yen><MkNumber :value="yenTotal ?? score"/></template>
							</I18n>
						</div>
						<I18n v-if="baseMode === 'sweets'" :src="i18n.ts._bubbleGame._score.scoreSweets" tag="div">
							<template #onigiriQtyWithUnit>
								<I18n :src="i18n.ts._bubbleGame._score.estimatedQty" tag="b">
									<template #qty><MkNumber :value="score / 130"/></template>
								</I18n>
							</template>
						</I18n>
					</div>
				</div>
				<div v-if="replaying" :class="$style.replayIndicator"><span :class="$style.replayIndicatorText"><i class="ti ti-player-play"></i> {{ i18n.ts.replaying }}</span></div>
			</div>

			<div v-if="replaying" class="_woodenFrame">
				<div class="_woodenFrameInner">
					<div style="background: #0004;">
						<div style="height: 10px; background: var(--MI_THEME-accent); will-change: width;" :style="{ width: `${(currentFrame / endedAtFrame) * 100}%` }"></div>
					</div>
				</div>
				<div class="_woodenFrameInner">
					<div class="_buttonsCenter">
						<MkButton @click="endReplay"><i class="ti ti-player-stop"></i> {{ i18n.ts.endReplay }}</MkButton>
						<MkButton :primary="replayPlaybackRate === 4" @click="replayPlaybackRate = replayPlaybackRate === 4 ? 1 : 4"><i class="ti ti-player-track-next"></i> x4</MkButton>
						<MkButton :primary="replayPlaybackRate === 16" @click="replayPlaybackRate = replayPlaybackRate === 16 ? 1 : 16"><i class="ti ti-player-track-next"></i> x16</MkButton>
					</div>
				</div>
			</div>

			<div v-if="isGameOver" class="_woodenFrame">
				<div class="_woodenFrameInner">
					<!-- mk-go (#3231): 対戦ではタイトルへ戻る・共有は対戦の画面が受け持つ。 -->
					<div v-if="versus != null" class="_buttonsCenter">
						<MkButton primary rounded @click="replay">{{ i18n.ts.showReplay }}</MkButton>
					</div>
					<div v-else class="_buttonsCenter">
						<MkButton primary rounded @click="backToTitle">{{ i18n.ts.backToTitle }}</MkButton>
						<MkButton primary rounded @click="replay">{{ i18n.ts.showReplay }}</MkButton>
						<MkButton primary rounded @click="share">{{ i18n.ts.share }}</MkButton>
						<MkButton rounded @click="exportLog">{{ i18n.ts.copyReplayData }}</MkButton>
					</div>
				</div>
			</div>

			<div style="display: flex;">
				<div class="_woodenFrame" style="flex: 1; margin-right: 10px;">
					<div class="_woodenFrameInner">
						<div>{{ i18n.ts._bubbleGame._score.score }}: <MkNumber :value="score"/>{{ dropAndFusionScoreUnit(gameMode) }}</div>
						<div v-if="versus == null">{{ i18n.ts._bubbleGame._score.highScore }}: <b v-if="highScore"><MkNumber :value="highScore"/>{{ dropAndFusionScoreUnit(gameMode) }}</b><b v-else>-</b></div>
						<div v-else>{{ i18n.ts._mkgoBubbleGame._versus.pendingStones }}: <b><MkNumber :value="pendingGarbage"/></b></div>
						<div v-if="baseMode === 'yen'">
							{{ i18n.ts._bubbleGame._score.scoreYen }}:
							<I18n :src="i18n.ts._bubbleGame._score.yen" tag="b">
								<template #yen><MkNumber :value="yenTotal ?? score"/></template>
							</I18n>
						</div>
					</div>
				</div>
				<div class="_woodenFrame" style="margin-left: auto;">
					<div class="_woodenFrameInner" style="text-align: center;">
						<div @click="showConfig = !showConfig"><i class="ti ti-settings"></i></div>
					</div>
				</div>
			</div>

			<div v-if="showConfig" class="_woodenFrame">
				<div class="_woodenFrameInner">
					<div class="_gaps">
						<MkRange v-model="bgmVolume" :min="0" :max="1" :step="0.01" :textConverter="(v) => `${Math.floor(v * 100)}%`" :continuousUpdate="true" @dragEnded="(v) => updateSettings('bgmVolume', v)">
							<template #label>BGM {{ i18n.ts.volume }}</template>
						</MkRange>
						<MkRange v-model="sfxVolume" :min="0" :max="1" :step="0.01" :textConverter="(v) => `${Math.floor(v * 100)}%`" :continuousUpdate="true" @dragEnded="(v) => updateSettings('sfxVolume', v)">
							<template #label>{{ i18n.ts.sfx }} {{ i18n.ts.volume }}</template>
						</MkRange>
					</div>
				</div>
			</div>

			<div class="_woodenFrame">
				<div class="_woodenFrameInner">
					<div>FUSION RECIPE</div>
					<div>
						<div v-for="(mono, i) in game.monoDefinitions.sort((a, b) => a.level - b.level)" :key="mono.id" style="display: inline-block;">
							<img :src="getTextureImageUrl(mono)" style="width: 32px; vertical-align: bottom;"/>
							<div v-if="i < game.monoDefinitions.length - 1" style="display: inline-block; margin-left: 4px; vertical-align: bottom;"><i class="ti ti-arrow-big-right"></i></div>
						</div>
					</div>
				</div>
			</div>

			<div class="_woodenFrame">
				<div class="_woodenFrameInner">
					<MkButton v-if="!isGameOver && !replaying" full danger @click="surrender">{{ i18n.ts.surrender }}</MkButton>
					<MkButton v-else-if="versus == null" full @click="restart">{{ i18n.ts.gameRetry }}</MkButton>
				</div>
			</div>
		</div>
	</div>
</div>
</template>

<script lang="ts" setup>
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref, shallowRef, watch, useTemplateRef } from 'vue';
import * as Matter from 'matter-js';
import * as Misskey from 'misskey-js';
import { DropAndFusionGame, parseGameMode } from 'misskey-bubble-game';
import { useInterval } from '@@/js/use-interval.js';
import { apiUrl } from '@@/js/config.js';
import type { GameMode, Mono } from 'misskey-bubble-game';
import { definePage } from '@/page.js';
import MkRippleEffect from '@/components/MkRippleEffect.vue';
import * as os from '@/os.js';
import MkNumber from '@/components/MkNumber.vue';
import MkPlusOneEffect from '@/components/MkPlusOneEffect.vue';
import MkButton from '@/components/MkButton.vue';
import { claimAchievement } from '@/utility/achievements.js';
import { store } from '@/store.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import { $i } from '@/i.js';
import * as sound from '@/utility/sound.js';
import MkRange from '@/components/MkRange.vue';
import { copyToClipboard } from '@/utility/copy-to-clipboard.js';
import { prefer } from '@/preferences.js';
import { clearDropAndFusionSave, writeDropAndFusionSave } from '@/utility/drop-and-fusion-save.js';
import { fastForwardGame } from '@/utility/drop-and-fusion-fast-forward.js';
import type { FastForwardResult } from '@/utility/drop-and-fusion-fast-forward.js';
import type { DropAndFusionSave } from '@/utility/drop-and-fusion-save.js';
import { dropAndFusionModeLabel, dropAndFusionScoreUnit } from '@/utility/drop-and-fusion-mode.js';
import type { VersusBoardState, VersusReport } from '@/utility/bubble-versus.js';
import { compactBoard } from '@/utility/bubble-versus-rules.js';
import { NORAML_MONOS, SQUARE_MONOS, SWEETS_MONOS, YEN_MONOS } from '@/utility/drop-and-fusion-monos.js';
import type { FrontendMonoDefinition } from '@/utility/drop-and-fusion-monos.js';

const props = defineProps<{
	// mk-go (#3216): 形と物理をつないだモードの文字列 (例: square-bouncy)。
	gameMode: GameMode;
	mute: boolean;
	/**
	 * A suspended game to continue from (mk-go, #3192). The caller has already
	 * checked that its version matches.
	 */
	resume?: DropAndFusionSave | null;
	/**
	 * Versus settings (mk-go, #3231). When set, the game uses the given seed,
	 * waits until `startAtLocal` (this device's clock) before it starts, does not
	 * save or register scores, and reports through the versus events.
	 */
	versus?: {
		seed: string;
		startAtLocal: number;
	} | null;
}>();

const emit = defineEmits<{
	(ev: 'end'): void;
	(ev: 'versusAttack', count: number): void;
	(ev: 'versusFinished', report: VersusReport): void;
}>();

// mk-go (#3216): 玉の見た目・単位・スコアの表示は形で決まる (物理では変わらない)。
const baseMode = computed(() => parseGameMode(props.gameMode)?.base ?? 'normal');

const monoDefinitions = computed(() => {
	return baseMode.value === 'normal' ? NORAML_MONOS :
		baseMode.value === 'square' ? SQUARE_MONOS :
		baseMode.value === 'yen' ? YEN_MONOS :
		baseMode.value === 'sweets' ? SWEETS_MONOS :
		baseMode.value === 'space' ? NORAML_MONOS :
		[] as never;
});

function getMonoRenderOptions(mono: Mono) {
	const def = monoDefinitions.value.find(x => x.id === mono.id)!;
	return {

		sprite: {
			texture: def.img,
			xScale: (mono.sizeX / def.imgSizeX) * def.spriteScale,
			yScale: (mono.sizeY / def.imgSizeY) * def.spriteScale,
		},

	};
}

let viewScale = 1;
// mk-go (#3231): 対戦では両者が同じシードで遊ぶ (サーバーが配る)。
let seed: string = props.versus?.seed ?? Date.now().toString();
let containerElRect: DOMRect | null = null;
let logs: ReturnType<DropAndFusionGame['getLogs']> | null = null;
let endedAtFrame = 0;
let bgmNodes: ReturnType<typeof sound.createSourceNode> | null = null;
let renderer: Matter.Render | null = null;
let monoTextures: Record<string, Blob> = {};
let monoTextureUrls: Record<string, string> = {};
let tickRaf: number | null = null;

function newGame() {
	return new DropAndFusionGame({
		seed: seed,
		gameMode: props.gameMode,
		getMonoRenderOptions,
		// mk-go (#3231): 対戦ではおじゃま石を受け、合体で攻撃を出す。リプレイも同じ
		// 設定で作らないと、石の出る記録が再現しない。
		versus: props.versus != null,
		getStoneRenderOptions,
	});
}

// おじゃま石は形によらず灰色の円で描く (玉の画像と見分けが付くように)。
function getStoneRenderOptions() {
	return {
		fillStyle: '#8a8a8a',
		strokeStyle: '#5a5a5a',
		lineWidth: 2,
	};
}

let game = newGame();
attachGameEvents();

const containerEl = useTemplateRef('containerEl');
const canvasEl = useTemplateRef('canvasEl');
const dropperX = ref(0);
const currentPick = shallowRef<{ id: string; mono: Mono } | null>(null);
const stock = shallowRef<{ id: string; mono: Mono }[]>([]);
const holdingStock = shallowRef<{ id: string; mono: Mono } | null>(null);
const score = ref(0);
const combo = ref(0);
const comboPrev = ref(0);
const maxCombo = ref(0);
const dropReady = ref(true);
const isGameOver = ref(false);
const gameLoaded = ref(false);
const readyGo = ref<'ready' | 'go' | null>('ready');
const highScore = ref<number | null>(null);
// mk-go (#3216): YEN は物理が違っても累計を 1 つにまとめる (yen-bouncy / yen-friction の
// 稼ぎも同じ財布に入る)。ハイスコアとランキングは組み合わせごとに分かれる。
const yenTotal = ref<number | null>(null);
const showConfig = ref(false);
const replaying = ref(false);
const replayPlaybackRate = ref(1);
const currentFrame = ref(0);
// mk-go (#3193): 判定領域に玉がとどまっている間 true。
const overflowWarning = ref(false);
// mk-go (#3231): 対戦で降るのを待っているおじゃま石の数。
const pendingGarbage = ref(0);
// 対戦で制限時間が来て止めた。ゲームオーバーとは別に表示する。
const timeUp = ref(false);
// 対戦の結果を 1 回だけ親へ渡すための印。
let versusFinished = false;
// 降参からのゲームオーバーを、普通のゲームオーバーと分けて報告する。
let surrendering = false;
// 対戦で終わったときの得点。
let finalScore: number | null = null;
// mk-go (#3192): 途中保存からの早送りの進捗 (0-1)。早送り中でなければ null。
const fastForwardProgress = ref<number | null>(null);
// 早送り中は効果音・演出・実績・保存を止める。リアクティブにする必要は無い
// (イベントの中で読むだけ)。
let fastForwarding = false;
// dispose のたびに進める。start の途中 (テクスチャの読み込み・早送り) で画面を離れたかを見る。
let generation = 0;
let deactivated = false;
// 途中保存は最初の start で 1 回だけ使う (リトライで同じ局面に戻らないように)。
let pendingResume: DropAndFusionSave | null = props.resume ?? null;
const bgmVolume = ref(prefer.s['game.dropAndFusion'].bgmVolume);
const sfxVolume = ref(prefer.s['game.dropAndFusion'].sfxVolume);

watch(replayPlaybackRate, (newValue) => {
	game.replayPlaybackRate = newValue;
});

watch(bgmVolume, (newValue) => {
	if (bgmNodes) {
		bgmNodes.gainNode.gain.value = props.mute ? 0 : newValue;
	}
});

function createRendererInstance(game: DropAndFusionGame) {
	return Matter.Render.create({
		engine: game.engine,
		canvas: canvasEl.value!,
		options: {
			width: game.GAME_WIDTH,
			height: game.GAME_HEIGHT,
			background: 'transparent', // transparent to hide
			wireframeBackground: 'transparent', // transparent to hide
			wireframes: false,
			showSleeping: false,
			pixelRatio: Math.max(2, window.devicePixelRatio),
		},
	});
}

function loadMonoTextures() {
	async function loadSingleMonoTexture(mono: FrontendMonoDefinition) {
		if (renderer == null) return;

		// Matter-js内にキャッシュがある場合はスキップ
		if (renderer.textures[mono.img]) return;

		let src = mono.img;

		if (monoTextureUrls[mono.img]) {
			src = monoTextureUrls[mono.img];
			// eslint-disable-next-line @typescript-eslint/no-unnecessary-condition
		} else if (monoTextures[mono.img]) {
			src = URL.createObjectURL(monoTextures[mono.img]);
			monoTextureUrls[mono.img] = src;
		} else {
			const res = await window.fetch(mono.img);
			const blob = await res.blob();
			monoTextures[mono.img] = blob;
			src = URL.createObjectURL(blob);
			monoTextureUrls[mono.img] = src;
		}

		const image = new Image();
		image.src = src;
		renderer.textures[mono.img] = image;
	}

	return Promise.all(monoDefinitions.value.map(x => loadSingleMonoTexture(x)));
}

function getTextureImageUrl(mono: Mono) {
	const def = monoDefinitions.value.find(x => x.id === mono.id)!;

	if (monoTextureUrls[def.img]) {
		return monoTextureUrls[def.img];

		// eslint-disable-next-line @typescript-eslint/no-unnecessary-condition
	} else if (monoTextures[def.img]) {
		// Gameクラス内にキャッシュがある場合はそれを使う
		const out = URL.createObjectURL(monoTextures[def.img]);
		monoTextureUrls[def.img] = out;
		return out;
	} else {
		return def.img;
	}
}

function tick() {
	const hasNextTick = game.tick();
	if (hasNextTick) {
		tickRaf = window.requestAnimationFrame(tick);
	} else {
		tickRaf = null;
	}
}

function tickReplay() {
	let hasNextTick;
	for (let i = 0; i < replayPlaybackRate.value; i++) {
		// mk-go: 同じフレームの操作は全部当てる (保持してすぐ落とすと 2 つになる)。
		// 本家は find で最初の 1 つだけを当てていて、途中保存の早送り (#3192) と結末が
		// ずれる。
		for (const log of logs!.filter(x => x.frame === game.frame)) {
			// 種類ごとの分岐はエンジン側 (applyLog) に任せる (#3229)。ここで書くと、
			// 種類が増えたときに取りこぼす。
			game.applyLog(log);
		}

		hasNextTick = game.tick();
		currentFrame.value = game.frame;
		// 時間切れで止めた対局はゲームオーバーにならないので、止めたフレームで終える。
		if (timeUp.value && game.frame >= endedAtFrame) {
			endReplay();
			return;
		}
		if (!hasNextTick) break;
	}

	if (hasNextTick) {
		tickRaf = window.requestAnimationFrame(tickReplay);
	} else {
		tickRaf = null;
	}
}

async function start() {
	const resume = pendingResume;
	pendingResume = null;
	const myGeneration = generation;
	if (resume != null) {
		// 同じシードでゲームを作り直す。setup で作ったゲームはまだ始まっていない。
		game.dispose();
		seed = resume.s;
		game = newGame();
		attachGameEvents();
	}

	renderer = createRendererInstance(game);
	await loadMonoTextures();
	Matter.Render.lookAt(renderer, {
		min: { x: 0, y: 0 },
		max: { x: game.GAME_WIDTH, y: game.GAME_HEIGHT },
	});
	// 待っている間に画面を離れた (dispose された) ら何もしない。続けると、壁も床も
	// 消えたゲームを描画と tick が回し続ける。
	if (generation !== myGeneration) return;
	game.start();
	if (resume != null) {
		// **描画を始める前に早送りする。** Render.run の後だと早送りの途中を毎フレーム
		// 描いてしまう。
		fastForwarding = true;
		fastForwardProgress.value = 0;
		let result: FastForwardResult;
		try {
			result = await fastForwardGame(game, DropAndFusionGame.deserializeLogs(resume.l), {
				isCancelled: () => generation !== myGeneration,
				onProgress: p => { fastForwardProgress.value = p; },
			});
		} finally {
			fastForwarding = false;
			fastForwardProgress.value = null;
		}
		if (result === 'cancelled') return;
		if (result === 'gameOver') {
			// 早送りの途中で終わった (記録と結末がずれた)。盤面は見せるが、tick も
			// 「GO」も始めない。登録と保存の削除は gameOver のイベントで済んでいる。
			Matter.Render.run(renderer);
			gameLoaded.value = true;
			readyGo.value = null;
			return;
		}
	}
	Matter.Render.run(renderer);
	if (props.versus != null) {
		// mk-go (#3231): 対戦は両者が同時に始める。開始時刻までは「READY」を出して待つ。
		gameLoaded.value = true;
		readyGo.value = 'ready';
		const wait = Math.max(0, props.versus.startAtLocal - Date.now());
		await new Promise(resolve => window.setTimeout(resolve, wait));
		if (generation !== myGeneration) return;
		tickRaf = window.requestAnimationFrame(tick);
		readyGo.value = 'go';
		window.setTimeout(() => {
			readyGo.value = null;
		}, 1000);
		return;
	}
	// mk-go: 最初の rAF も tickRaf に控える。控えないと、この 1 フレームの間に dispose
	// されたとき cancel されずに tick が回り続ける。
	tickRaf = window.requestAnimationFrame(tick);

	gameLoaded.value = true;

	window.setTimeout(() => {
		readyGo.value = 'go';
		window.setTimeout(() => {
			readyGo.value = null;
		}, 1000);
	}, 1500);
}

/**
 * Saves the game so far, so it can be continued after a reload (mk-go, #3192).
 *
 * 操作の記録が増えるのは落としたときと保持したときだけなので、そのたびに保存すれば
 * 最後の状態まで戻せる。
 */
function saveProgress() {
	if (replaying.value || fastForwarding || isGameOver.value) return;
	// 対戦は途中から再開できない (相手の盤面と揃わなくなる)。
	if (props.versus != null) return;
	writeDropAndFusionSave({
		v: game.GAME_VERSION,
		m: props.gameMode,
		s: seed,
		l: DropAndFusionGame.serializeLogs(game.getLogs()),
	});
}

function onClick(ev: PointerEvent) {
	if (!containerElRect) return;
	// 対戦の時間切れはエンジンの外で止めるので、終わった後の操作もここで止める。
	if (replaying.value || isGameOver.value) return;
	const x = (ev.clientX - containerElRect.left) / viewScale;
	game.drop(x);
}

function onTouchend(ev: TouchEvent) {
	if (!containerElRect) return;
	if (replaying.value || isGameOver.value) return;
	const x = (ev.changedTouches[0].clientX - containerElRect.left) / viewScale;
	game.drop(x);
}

function onMousemove(ev: MouseEvent) {
	if (!containerElRect) return;
	const x = (ev.clientX - containerElRect.left);
	moveDropper(containerElRect, x);
}

function onTouchmove(ev: TouchEvent) {
	if (!containerElRect) return;
	const x = (ev.touches[0].clientX - containerElRect.left);
	moveDropper(containerElRect, x);
}

function moveDropper(rect: DOMRect, x: number) {
	dropperX.value = Math.min(rect.width * ((game.GAME_WIDTH - game.PLAYAREA_MARGIN) / game.GAME_WIDTH), Math.max(rect.width * (game.PLAYAREA_MARGIN / game.GAME_WIDTH), x));
}

function hold() {
	if (replaying.value || isGameOver.value) return;
	game.hold();
}

async function surrender() {
	const { canceled } = await os.confirm({
		type: 'warning',
		text: i18n.ts.areYouSure,
	});
	if (canceled) return;
	surrendering = true;
	game.surrender();
}

async function restart() {
	reset();
	game = newGame();
	attachGameEvents();
	await start();
}

function reset() {
	dispose();
	seed = Date.now().toString();
	isGameOver.value = false;
	replaying.value = false;
	replayPlaybackRate.value = 1;
	currentPick.value = null;
	dropReady.value = true;
	stock.value = [];
	holdingStock.value = null;
	score.value = 0;
	combo.value = 0;
	comboPrev.value = 0;
	maxCombo.value = 0;
	gameLoaded.value = false;
	readyGo.value = null;
	overflowWarning.value = false;
}

function dispose() {
	// 進行中の start / 早送りを止める合図。
	generation++;
	game.dispose();
	if (renderer) Matter.Render.stop(renderer);
	if (tickRaf) {
		window.cancelAnimationFrame(tickRaf);
	}
}

function backToTitle() {
	emit('end');
}

function replay() {
	replaying.value = true;
	dispose();
	game = newGame();
	attachGameEvents();
	const myGeneration = generation;
	os.promiseDialog(loadMonoTextures(), async () => {
		// mk-go: 読み込みの間に画面を離れたら始めない (壁の無いゲームを回し続ける)。
		if (generation !== myGeneration) return;
		renderer = createRendererInstance(game);
		Matter.Render.lookAt(renderer, {
			min: { x: 0, y: 0 },
			max: { x: game.GAME_WIDTH, y: game.GAME_HEIGHT },
		});
		Matter.Render.run(renderer);
		game.start();
		window.requestAnimationFrame(tickReplay);
	});
}

function endReplay() {
	replaying.value = false;
	dispose();
}

function exportLog() {
	if (!logs) return;
	const data = JSON.stringify({
		v: game.GAME_VERSION,
		m: props.gameMode,
		s: seed,
		d: new Date().toISOString(),
		l: DropAndFusionGame.serializeLogs(logs),
	});
	copyToClipboard(data);
}

function updateSettings<
	K extends keyof typeof prefer.s['game.dropAndFusion'],
	V extends typeof prefer.s['game.dropAndFusion'][K],
>(key: K, value: V) {
	const changes: { [P in K]?: V } = {};
	changes[key] = value;
	prefer.commit('game.dropAndFusion', {
		...prefer.s['game.dropAndFusion'],
		...changes,
	});
}

function loadImage(url: string) {
	return new Promise<HTMLImageElement>(res => {
		const img = new Image();
		img.src = url;
		img.addEventListener('load', () => {
			res(img);
		});
	});
}

function getGameImageDriveFile() {
	return new Promise<Misskey.entities.DriveFile | null>(res => {
		const dcanvas = window.document.createElement('canvas');
		dcanvas.width = game.GAME_WIDTH;
		dcanvas.height = game.GAME_HEIGHT;
		const ctx = dcanvas.getContext('2d');
		if (!ctx || !canvasEl.value) return res(null);
		Promise.all([
			loadImage('/client-assets/drop-and-fusion/frame-light.svg'),
			loadImage('/client-assets/drop-and-fusion/logo.png'),
		]).then((images) => {
			const [frame, logo] = images;
			ctx.fillStyle = '#fff';
			ctx.fillRect(0, 0, game.GAME_WIDTH, game.GAME_HEIGHT);

			ctx.drawImage(frame, 0, 0, game.GAME_WIDTH, game.GAME_HEIGHT);
			ctx.drawImage(canvasEl.value!, 0, 0, game.GAME_WIDTH, game.GAME_HEIGHT);

			ctx.fillStyle = '#000';
			ctx.font = '16px bold sans-serif';
			ctx.textBaseline = 'top';
			ctx.fillText(`SCORE: ${score.value.toLocaleString()}${dropAndFusionScoreUnit(props.gameMode)}`, 10, 10);

			ctx.globalAlpha = 0.7;
			ctx.drawImage(logo, game.GAME_WIDTH * 0.55, 6, game.GAME_WIDTH * 0.45, game.GAME_WIDTH * 0.45 * (logo.height / logo.width));
			ctx.globalAlpha = 1;

			dcanvas.toBlob(blob => {
				if (!blob) return res(null);
				if ($i == null) return res(null);
				const formData = new FormData();
				formData.append('file', blob);
				formData.append('name', `bubble-game-${Date.now()}.png`);
				formData.append('isSensitive', 'false');
				formData.append('i', $i.token);
				if (prefer.s.uploadFolder) {
					formData.append('folderId', prefer.s.uploadFolder);
				}

				window.fetch(apiUrl + '/drive/files/create', {
					method: 'POST',
					body: formData,
				})
					.then(response => response.json())
					.then(f => {
						res(f);
					});
			}, 'image/png');

			dcanvas.remove();
		});
	});
}

async function share() {
	const uploading = getGameImageDriveFile();
	os.promiseDialog(uploading);
	const file = await uploading;
	if (!file) return;
	os.post({
		initialText: `#BubbleGame (${props.gameMode})
SCORE: ${score.value.toLocaleString()}${dropAndFusionScoreUnit(props.gameMode)}`,
		initialFiles: [file],
		instant: true,
	});
}

function attachGameEvents() {
	game.addListener('overflowWarning', value => {
		overflowWarning.value = value;
	});

	game.addListener('changePendingGarbage', value => {
		pendingGarbage.value = value;
	});

	game.addListener('attack', count => {
		// リプレイと早送りでは攻撃を送らない (もう終わった対局の記録なので)。
		if (props.versus == null || replaying.value || fastForwarding || versusFinished) return;
		emit('versusAttack', count);
	});

	game.addListener('changeScore', value => {
		score.value = value;
	});

	game.addListener('changeCombo', value => {
		if (value === 0) {
			comboPrev.value = combo.value;
		} else {
			comboPrev.value = value;
		}
		maxCombo.value = Math.max(maxCombo.value, value);
		combo.value = value;
	});

	game.addListener('changeStock', value => {
		currentPick.value = JSON.parse(JSON.stringify(value[0]));
		stock.value = JSON.parse(JSON.stringify(value.slice(1)));
	});

	game.addListener('changeHolding', value => {
		holdingStock.value = value;
		saveProgress();

		if (!props.mute && !fastForwarding) {
			sound.playUrl('/client-assets/drop-and-fusion/hold.mp3', {
				volume: 0.5 * sfxVolume.value,
				playbackRate: replayPlaybackRate.value,
			});
		}
	});

	game.addListener('dropped', (x) => {
		saveProgress();

		if (!props.mute && !fastForwarding) {
			const panV = x - game.PLAYAREA_MARGIN;
			const panW = game.GAME_WIDTH - game.PLAYAREA_MARGIN - game.PLAYAREA_MARGIN;
			const pan = ((panV / panW) - 0.5) * 2;
			if (baseMode.value === 'yen') {
				sound.playUrl('/client-assets/drop-and-fusion/drop_yen.mp3', {
					volume: sfxVolume.value,
					pan,
					playbackRate: replayPlaybackRate.value,
				});
			} else {
				sound.playUrl('/client-assets/drop-and-fusion/drop.mp3', {
					volume: sfxVolume.value,
					pan,
					playbackRate: replayPlaybackRate.value,
				});
			}
		}

		if (replaying.value || fastForwarding) return;

		dropReady.value = false;
		window.setTimeout(() => {
			if (!isGameOver.value) {
				dropReady.value = true;
			}
		}, game.frameToMs(game.DROP_COOLTIME));
	});

	game.addListener('fusioned', (x, y, nextMono, scoreDelta) => {
		// 早送り中は演出も音も出さない (#3192)。
		if (fastForwarding) return;
		if (!canvasEl.value) return;

		const rect = canvasEl.value.getBoundingClientRect();
		const domX = rect.left + (x * viewScale);
		const domY = rect.top + (y * viewScale);
		const scoreUnit = dropAndFusionScoreUnit(props.gameMode);

		{
			const { dispose } = os.popup(MkRippleEffect, { x: domX, y: domY }, {
				end: () => dispose(),
			});
		}

		{
			const { dispose } = os.popup(MkPlusOneEffect, { x: domX, y: domY, value: scoreDelta + (scoreUnit === 'pt' ? '' : scoreUnit) }, {
				end: () => dispose(),
			});
		}

		if (nextMono) {
			const def = monoDefinitions.value.find(x => x.id === nextMono.id)!;
			if (!props.mute) {
				const panV = x - game.PLAYAREA_MARGIN;
				const panW = game.GAME_WIDTH - game.PLAYAREA_MARGIN - game.PLAYAREA_MARGIN;
				const pan = ((panV / panW) - 0.5) * 2;
				const pitch = def.sfxPitch;
				if (baseMode.value === 'yen') {
					sound.playUrl('/client-assets/drop-and-fusion/fusion_yen.mp3', {
						volume: 0.25 * sfxVolume.value,
						pan: pan,
						playbackRate: (pitch / 4) * replayPlaybackRate.value,
					});
				} else {
					sound.playUrl('/client-assets/drop-and-fusion/fusion.mp3', {
						volume: sfxVolume.value,
						pan: pan,
						playbackRate: pitch * replayPlaybackRate.value,
					});
				}
			}
		} else {
			if (!props.mute) {
				// TODO: 融合後のモノがない場合でも何らかの効果音を再生
			}
		}
	});

	const minCollisionEnergyForSound = 2.5;
	const maxCollisionEnergyForSound = 9;
	const soundPitchMax = 4;
	const soundPitchMin = 0.5;

	game.addListener('collision', (energy, bodyA, bodyB) => {
		if (!props.mute && !fastForwarding && (energy > minCollisionEnergyForSound)) {
			const volume = (Math.min(maxCollisionEnergyForSound, energy - minCollisionEnergyForSound) / maxCollisionEnergyForSound) / 4;
			const panV =
				bodyA.label === '_wall_' ? bodyB.position.x - game.PLAYAREA_MARGIN :
				bodyB.label === '_wall_' ? bodyA.position.x - game.PLAYAREA_MARGIN :
				((bodyA.position.x + bodyB.position.x) / 2) - game.PLAYAREA_MARGIN;
			const panW = game.GAME_WIDTH - game.PLAYAREA_MARGIN - game.PLAYAREA_MARGIN;
			const pan = ((panV / panW) - 0.5) * 2;
			const pitch = soundPitchMin + ((soundPitchMax - soundPitchMin) * (1 - (Math.min(10, energy) / 10)));

			if (baseMode.value === 'yen') {
				sound.playUrl('/client-assets/drop-and-fusion/collision_yen.mp3', {
					volume: volume * sfxVolume.value,
					pan: pan,
					playbackRate: Math.max(1, pitch) * replayPlaybackRate.value,
				});
			} else {
				sound.playUrl('/client-assets/drop-and-fusion/collision.mp3', {
					volume: volume * sfxVolume.value,
					pan: pan,
					playbackRate: pitch * replayPlaybackRate.value,
				});
			}
		}
	});

	game.addListener('monoAdded', (mono) => {
		if (replaying.value || fastForwarding) return;

		// 実績関連
		if (mono.level === 10) {
			claimAchievement('bubbleGameExplodingHead');

			const monos = game.getActiveMonos();
			if (monos.filter(x => x.level === 10).length >= 2) {
				claimAchievement('bubbleGameDoubleExplodingHead');
			}
		}
	});

	game.addListener('gameOver', () => {
		if (!props.mute) {
			if (baseMode.value === 'yen') {
				sound.playUrl('/client-assets/drop-and-fusion/gameover_yen.mp3', {
					volume: 0.5 * sfxVolume.value,
				});
			} else {
				sound.playUrl('/client-assets/drop-and-fusion/gameover.mp3', {
					volume: sfxVolume.value,
				});
			}
		}

		if (replaying.value) {
			endReplay();
			return;
		}

		logs = game.getLogs();
		endedAtFrame = game.frame;
		currentPick.value = null;
		dropReady.value = false;
		isGameOver.value = true;

		if (props.versus != null) {
			// mk-go (#3231): 対戦ではランキングにもハイスコアにも載せない (別の遊び方の
			// 得点なので)。結果は親が対局の報告として送る。
			finishVersus(surrendering ? 'surrender' : 'gameOver');
			return;
		}

		// 終わったゲームは再開しない (#3192)。降参もここを通る。
		clearDropAndFusionSave(props.gameMode);

		misskeyApi('bubble-game/register', {
			seed,
			score: score.value,
			gameMode: props.gameMode,
			gameVersion: game.GAME_VERSION,
			logs: DropAndFusionGame.serializeLogs(logs),
		});

		if (baseMode.value === 'yen') {
			yenTotal.value = (yenTotal.value ?? 0) + score.value;
			misskeyApi('i/registry/set', {
				scope: ['dropAndFusionGame'],
				key: 'yenTotal',
				value: yenTotal.value,
			});
		}

		if (score.value > (highScore.value ?? 0)) {
			highScore.value = score.value;

			misskeyApi('i/registry/set', {
				scope: ['dropAndFusionGame'],
				key: 'highScore:' + props.gameMode,
				value: highScore.value,
			});
		}
	});
}

function finishVersus(reason: VersusReport['reason']) {
	if (versusFinished) return;
	versusFinished = true;
	finalScore = score.value;
	emit('versusFinished', {
		score: score.value,
		frame: game.frame,
		reason,
		logs: DropAndFusionGame.serializeLogs(game.getLogs()),
		// mk-go (#3232): 記録はこの版のエンジンでしか同じ結末に再生できない。
		gameVersion: game.GAME_VERSION,
	});
}

/**
 * Stops the versus game when the time limit is reached (mk-go, #3231).
 * Does nothing when the game is already over.
 */
function finishByTimeUp() {
	if (props.versus == null || versusFinished || isGameOver.value) return;
	if (tickRaf) {
		window.cancelAnimationFrame(tickRaf);
		tickRaf = null;
	}
	logs = game.getLogs();
	endedAtFrame = game.frame;
	currentPick.value = null;
	dropReady.value = false;
	timeUp.value = true;
	isGameOver.value = true;
	finishVersus('timeUp');
}

/**
 * Stops the versus game because the opponent's report ended the match, and
 * reports this board for the replay (mk-go, #3232). Does nothing when this
 * side has already finished.
 */
function finishByOpponentEnded() {
	if (props.versus == null || versusFinished || isGameOver.value) return;
	if (tickRaf) {
		window.cancelAnimationFrame(tickRaf);
		tickRaf = null;
	}
	logs = game.getLogs();
	endedAtFrame = game.frame;
	currentPick.value = null;
	dropReady.value = false;
	isGameOver.value = true;
	finishVersus('opponentEnded');
}

/** Receives stones from the opponent (mk-go, #3231). */
function receiveAttack(count: number) {
	if (props.versus == null || versusFinished) return;
	game.receiveAttack(count);
}

/** A summary of the board to show the opponent (mk-go, #3231). */
function boardBodies() {
	const out: { x: number; y: number; r: number; level: number }[] = [];
	for (const body of game.engine.world.bodies) {
		const stone = body.label === DropAndFusionGame.STONE_LABEL;
		const mono = stone ? null : game.monoDefinitions.find(m => m.id === body.label);
		// 壁や判定領域は送らない。
		if (!stone && mono == null) continue;
		const r = body.circleRadius ?? ((body.bounds.max.x - body.bounds.min.x) / 2);
		out.push({ x: body.position.x, y: body.position.y, r, level: stone ? 0 : mono!.level });
	}
	return out;
}

function boardState(): VersusBoardState {
	return {
		board: compactBoard(boardBodies()),
		// 終わった後のリプレイで得点が変わっても、相手へは最後の得点を知らせる。
		score: finalScore ?? score.value,
		pending: pendingGarbage.value,
		danger: overflowWarning.value,
		gameOver: isGameOver.value,
	};
}

defineExpose({ finishByTimeUp, finishByOpponentEnded, receiveAttack, boardState });

useInterval(() => {
	if (!canvasEl.value) return;
	const actualCanvasWidth = canvasEl.value.getBoundingClientRect().width;
	if (actualCanvasWidth === 0) return;
	viewScale = actualCanvasWidth / game.GAME_WIDTH;
	containerElRect = containerEl.value?.getBoundingClientRect() ?? null;
}, 1000, { immediate: false, afterMounted: true });

onMounted(async () => {
	// mk-go: 読み込み (途中保存の早送りを含む) の間に画面を離れると dispose される。
	// その後で BGM を流すと、止める人がいない (onUnmounted はもう走った) のでタイトルや
	// 他のページで鳴り続ける。
	const mountedGeneration = generation;
	// 対戦の得点はハイスコアに載せないので読まない。
	if (props.versus == null) {
		try {
			// レジストリの値の形は書いた側しか知らないので、ここで表明する (#3439)
			highScore.value = await misskeyApi('i/registry/get', {
				scope: ['dropAndFusionGame'],
				key: 'highScore:' + props.gameMode,
			}) as number;
		} catch (err) {
			highScore.value = null;
		}
	}

	if (baseMode.value === 'yen' && props.versus == null) {
		try {
			yenTotal.value = await misskeyApi('i/registry/get', {
				scope: ['dropAndFusionGame'],
				key: 'yenTotal',
			}) as number;
		} catch (err: any) {
			if (err.code === 'NO_SUCH_KEY') {
				// nop
			} else {
				os.alert({
					type: 'error',
					text: i18n.ts.cannotLoad,
				});
				return;
			}
		}
	}

	/*
	const getVerticesFromSvg = async (path: string) => {
		const svgDoc = await fetch(path)
			.then((response) => response.text())
			.then((svgString) => {
				const parser = new DOMParser();
				return parser.parseFromString(svgString, 'image/svg+xml');
			});
		const pathDatas = svgDoc.querySelectorAll('path');
		if (!pathDatas) return;
		const vertices = Array.from(pathDatas).map((pathData) => {
			return Matter.Svg.pathToVertices(pathData);
		});
		return vertices;
	};

	getVerticesFromSvg('/client-assets/drop-and-fusion/sweets_monos/verts/doughnut_color.svg').then((vertices) => {
		console.log('doughnut_color', vertices);
	});
	*/

	if (generation !== mountedGeneration) return;
	await start();
	if (generation !== mountedGeneration) return;

	const bgmBuffer = await sound.loadAudio('/client-assets/drop-and-fusion/bgm_1.mp3');
	if (!bgmBuffer) return;
	if (generation !== mountedGeneration) return;
	bgmNodes = sound.createSourceNode(bgmBuffer, {
		volume: props.mute ? 0 : bgmVolume.value,
	});
	if (!bgmNodes) return;
	bgmNodes.soundSource.loop = true;
	bgmNodes.soundSource.start();
});

onUnmounted(() => {
	dispose();
	bgmNodes?.soundSource.stop();
});

onDeactivated(() => {
	dispose();
	bgmNodes?.soundSource.stop();
	deactivated = true;
});

// mk-go: ページはキャッシュされる (KeepAlive) ので、離れて戻ると片付け済みのゲームが
// そのまま出てくる (読み込み中のまま抜けられない / 壁の無い盤面)。タイトルへ戻す。
// 途中保存は残っているので「続きから」で同じ局面に戻れる (#3192)。
onActivated(() => {
	if (!deactivated) return;
	deactivated = false;
	emit('end');
});

definePage(() => ({
	title: i18n.ts.bubbleGame,
	icon: 'ti ti-apple',
}));
</script>

<style lang="scss" module>
.timeUpLabel {
	font-size: 2em;
	font-weight: bold;
	letter-spacing: 0.1em;
}

.transition_zoom_move,
.transition_zoom_enterActive,
.transition_zoom_leaveActive {
	transition: opacity 0.5s cubic-bezier(0,.5,.5,1), transform 0.5s cubic-bezier(0,.5,.5,1) !important;
}
.transition_zoom_enterFrom,
.transition_zoom_leaveTo {
	opacity: 0;
	transform: scale(0.8);
}

.transition_stock_move,
.transition_stock_enterActive,
.transition_stock_leaveActive {
	transition: opacity 0.4s cubic-bezier(0,.5,.5,1), transform 0.4s cubic-bezier(0,.5,.5,1) !important;
}
.transition_stock_enterFrom,
.transition_stock_leaveTo {
	opacity: 0;
	transform: scale(0.7);
}
.transition_stock_leaveActive {
	position: absolute;
}

.transition_picked_move,
.transition_picked_enterActive {
	transition: opacity 0.5s cubic-bezier(0,.5,.5,1), transform 0.5s cubic-bezier(0,.5,.5,1) !important;
}
.transition_picked_leaveActive {
	transition: all 0s !important;
}
.transition_picked_enterFrom,
.transition_picked_leaveTo {
	opacity: 0;
	transform: translateY(-50px);
}
.transition_picked_leaveActive {
	position: absolute;
}

.transition_combo_move,
.transition_combo_enterActive {
	transition: all 0s !important;
}
.transition_combo_leaveActive {
	transition: opacity 0.4s cubic-bezier(0,.5,.5,1), transform 0.4s cubic-bezier(0,.5,.5,1) !important;
}
.transition_combo_enterFrom,
.transition_combo_leaveTo {
	opacity: 0;
	transform: scale(0.7);
}
.transition_combo_leaveActive {
	position: absolute;
}

.root {
	margin: 0 auto;
	max-width: 600px;
	user-select: none;

	* {
		user-select: none;
	}
}

.loadingScreen {
	text-align: center;
	padding: 32px;
}

.readyGo_bg {
	position: absolute;
	top: 0;
	left: 0;
	right: 0;
	bottom: 0;
	z-index: 100;
	backdrop-filter: blur(4px);
}

.readyGo_ready,
.readyGo_go {
	position: absolute;
	top: 0;
	left: 0;
	right: 0;
	bottom: 0;
	display: flex;
	align-items: center;
	justify-content: center;
	z-index: 101;
	pointer-events: none;
}

.readyGo_img {
	display: block;
	width: 250px;
	max-width: 100%;
}

.header {
	position: relative;
	z-index: 10;
	display: grid;
	grid-template-columns: 1fr;
	grid-template-rows: auto auto;
	gap: 8px;

	> .headerTitle {
		text-align: center;
	}

	@media (min-width: 500px) {
		grid-template-columns: 1fr auto;
		grid-template-rows: auto;

		> .headerTitle {
			text-align: start;
		}
	}
}

.mainFrameImg {
	position: absolute;
	top: 0;
	left: 0;
	width: 100%;
	// なんかiOSでちらつく
	//filter: drop-shadow(0 6px 16px #0007);
	pointer-events: none;
	user-select: none;
}

.canvas {
	position: relative;
	display: block;
	z-index: 1;
	width: 100% !important;
	height: auto !important;
	pointer-events: none;
	user-select: none;
}

.gameContainer {
	position: relative;
	margin-top: -20px;
}

.stock {
	pointer-events: none;
	user-select: none;
}

.combo {
	position: absolute;
	z-index: 3;
	top: 50%;
	width: 100%;
	text-align: center;
	font-weight: bold;
	font-style: oblique;
	color: #fff;
	-webkit-text-stroke: 1px rgb(255, 145, 0);
	text-shadow: 0 0 6px #0005;
	pointer-events: none;
	user-select: none;
}

.dropperContainer {
	position: absolute;
	top: 0;
	height: 100%;
	z-index: 2;
	pointer-events: none;
	user-select: none;
	will-change: left;
}

.currentMono {
	position: absolute;
	display: block;
	bottom: 88%;
	z-index: 2;
	filter: drop-shadow(0 6px 16px #0007);
}

.dropper {
	position: relative;
	top: 0;
	width: 70px;
	margin-top: -10px;
	margin-left: -30px;
	z-index: 2;
	filter: drop-shadow(0 6px 16px #0007);
}

.currentMonoArrow {
	position: absolute;
	width: 20px;
	bottom: 80%;
	left: -10px;
	z-index: 3;
	animation: currentMonoArrow 2s ease infinite;
}

.overflowWarning {
	// 判定領域 (y = -100..100) の下端。canvas の高さは GAME_HEIGHT (600) に対応する。
	position: absolute;
	z-index: 3;
	top: calc(100% * 100 / 600);
	left: 0;
	right: 0;
	height: 3px;
	margin-top: -1px;
	background: var(--MI_THEME-error);
	pointer-events: none;
	animation: overflowWarningBlink 0.5s steps(2, jump-none) infinite alternate;
}

@keyframes overflowWarningBlink {
	from { opacity: 1; }
	to { opacity: 0.2; }
}

.dropGuide {
	position: absolute;
	z-index: 3;
	bottom: 0;
	width: 3px;
	margin-left: -2px;
	height: 85%;
	background: #f002;
}

.gameOverLabel {
	position: absolute;
	z-index: 10;
	top: 50%;
	left: 0;
	right: 0;
	margin: auto;
	width: calc(100% - 50px);
	max-width: 320px;
	padding: 16px;
	box-sizing: border-box;
	background: #0007;
	border-radius: 16px;
	color: #fff;
	text-align: center;
	font-weight: bold;
}

.gameOver {
	.canvas {
		filter: grayscale(1);
	}
}

.replayIndicator {
	position: absolute;
	z-index: 10;
	left: 10px;
	bottom: 10px;
	padding: 6px 8px;
	color: #f00;
	font-weight: bold;
	background: #0008;
	border-radius: 6px;
	pointer-events: none;
}

.replayIndicatorText {
	animation: replayIndicator-blink 2s infinite;
}

@keyframes replayIndicator-blink {
	0% { opacity: 1; }
	50% { opacity: 0; }
	100% { opacity: 1; }
}

@keyframes currentMonoArrow {
	0% { transform: translateY(0); }
	25% { transform: translateY(-8px); }
	50% { transform: translateY(0); }
	75% { transform: translateY(-8px); }
	100% { transform: translateY(0); }
}
</style>
