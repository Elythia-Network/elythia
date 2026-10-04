<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go (#3232): 対戦のリプレイで、片方の盤面を記録から再生する。進め方は親が決め、
	この部品は「指定されたフレームまで進める」だけを受け持つ (2 枚を同じフレームで
	並べて進めるため)。
-->
<template>
<div :class="$style.root">
	<img v-if="store.s.darkMode" src="/client-assets/drop-and-fusion/frame-dark.svg" :class="$style.frameImg"/>
	<img v-else src="/client-assets/drop-and-fusion/frame-light.svg" :class="$style.frameImg"/>
	<canvas ref="canvasEl" :class="$style.canvas"></canvas>
</div>
</template>

<script lang="ts" setup>
import { computed, onMounted, onUnmounted, useTemplateRef } from 'vue';
import * as Matter from 'matter-js';
import { DropAndFusionGame, parseGameMode } from 'misskey-bubble-game';
import type { GameMode, Mono } from 'misskey-bubble-game';
import { store } from '@/store.js';
import { NORAML_MONOS, SQUARE_MONOS, SWEETS_MONOS, YEN_MONOS } from '@/utility/drop-and-fusion-monos.js';
import { createReplayCursor } from '@/utility/drop-and-fusion-replay.js';

const props = defineProps<{
	gameMode: GameMode;
	seed: string;
	/** The serialized logs of this board. */
	logs: number[][];
	/** The frame this board ended at (the reported frame). */
	endFrame: number;
}>();

const emit = defineEmits<{
	(ev: 'score', score: number): void;
}>();

const canvasEl = useTemplateRef('canvasEl');

const monoDefinitions = computed(() => {
	const base = parseGameMode(props.gameMode)?.base ?? 'normal';
	return base === 'yen' ? YEN_MONOS :
		base === 'square' ? SQUARE_MONOS :
		base === 'sweets' ? SWEETS_MONOS :
		NORAML_MONOS;
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

// 対局の画面と同じ見た目にする (石は灰色の円)。
function getStoneRenderOptions() {
	return {
		fillStyle: '#8a8a8a',
		strokeStyle: '#5a5a5a',
		lineWidth: 2,
	};
}

// 対局と同じく対戦の設定で作る。そうしないと、記録の石が降らず結末が変わる。
const game = new DropAndFusionGame({
	seed: props.seed,
	gameMode: props.gameMode,
	getMonoRenderOptions,
	versus: true,
	getStoneRenderOptions,
});
const cursor = createReplayCursor(game, DropAndFusionGame.deserializeLogs(props.logs), props.endFrame);
let renderer: Matter.Render | null = null;

game.on('changeScore', (score: number) => emit('score', score));

/**
 * Advances this board to `frame` (capped at the end frame). Returns false once
 * the board has ended.
 */
function advanceTo(frame: number): boolean {
	return cursor.advanceTo(frame);
}

onMounted(() => {
	renderer = Matter.Render.create({
		engine: game.engine,
		canvas: canvasEl.value!,
		options: {
			width: game.GAME_WIDTH,
			height: game.GAME_HEIGHT,
			background: 'transparent',
			wireframeBackground: 'transparent',
			wireframes: false,
			showSleeping: false,
			pixelRatio: Math.max(2, window.devicePixelRatio),
		},
	});
	Matter.Render.lookAt(renderer, {
		min: { x: 0, y: 0 },
		max: { x: game.GAME_WIDTH, y: game.GAME_HEIGHT },
	});
	Matter.Render.run(renderer);
	game.start();
});

onUnmounted(() => {
	game.dispose();
	if (renderer) Matter.Render.stop(renderer);
});

defineExpose({ advanceTo });
</script>

<style lang="scss" module>
.root {
	position: relative;
}

.frameImg {
	position: absolute;
	top: 0;
	left: 0;
	width: 100%;
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
</style>
