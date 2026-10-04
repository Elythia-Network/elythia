<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go (#3231): 対戦相手の盤面を小さく描く。相手から 1 秒ごとに届く玉の位置を
	色付きの丸で描くだけで、物理は動かさない (相手の盤面を再現するものではない)。
-->
<template>
<canvas ref="canvasEl" :class="$style.root" :width="WIDTH * PIXEL_RATIO" :height="HEIGHT * PIXEL_RATIO" role="img" :aria-label="i18n.ts._mkgoBubbleGame._versus.opponentBoard"></canvas>
</template>

<script lang="ts" setup>
import { onMounted, useTemplateRef, watch } from 'vue';
import { i18n } from '@/i18n.js';
import { parseBoard } from '@/utility/bubble-versus-rules.js';

const props = defineProps<{
	board: unknown;
	danger: boolean;
}>();

// ゲームの座標 (450x600) をこの大きさに縮める。
const GAME_WIDTH = 450;
const GAME_HEIGHT = 600;
const MARGIN = 25;
const WIDTH = 96;
const HEIGHT = 128;
const PIXEL_RATIO = 2;

// Lv ごとの色。石 (Lv0) は灰色。
const COLORS = ['#8a8a8a', '#f4a3c0', '#f7c873', '#f1e07a', '#b6e07a', '#7fd6b0', '#7fc3e8', '#9aa6f0', '#c69af0', '#f09ad2', '#f08a8a', '#e8b25a'];

const canvasEl = useTemplateRef('canvasEl');

function draw() {
	const canvas = canvasEl.value;
	const ctx = canvas?.getContext('2d');
	if (canvas == null || ctx == null) return;
	const scale = (WIDTH * PIXEL_RATIO) / GAME_WIDTH;
	ctx.clearRect(0, 0, canvas.width, canvas.height);

	// 箱。判定領域 (上端から 100) を危険のときは赤く塗る。
	ctx.fillStyle = props.danger ? 'rgba(240, 80, 80, 0.35)' : 'rgba(240, 120, 120, 0.15)';
	ctx.fillRect(MARGIN * scale, 0, (GAME_WIDTH - (MARGIN * 2)) * scale, 100 * scale);
	ctx.strokeStyle = 'rgba(120, 80, 40, 0.8)';
	ctx.lineWidth = 2;
	ctx.strokeRect(MARGIN * scale, 0, (GAME_WIDTH - (MARGIN * 2)) * scale, (GAME_HEIGHT - MARGIN) * scale);

	for (const b of parseBoard(props.board)) {
		ctx.beginPath();
		ctx.arc(b.x * scale, b.y * scale, Math.max(1, b.r * scale), 0, Math.PI * 2);
		ctx.fillStyle = COLORS[Math.min(Math.max(0, Math.floor(b.level)), COLORS.length - 1)];
		ctx.fill();
	}
}

watch(() => [props.board, props.danger], draw);
onMounted(draw);
</script>

<style lang="scss" module>
.root {
	display: block;
	width: 96px;
	height: 128px;
	border-radius: 4px;
	background: rgba(255, 245, 230, 0.8);
}
</style>
