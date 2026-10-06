<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	Elythia: エントランスの背景。サーバーの背景画像が設定されていればそれを出し、
	無ければ夜空に既定アイコンの絵を薄く溶かす。左の斜めの帯 (本家の意匠) の代わり。
-->
<template>
<MkFeaturedPhotos v-if="instance.backgroundImageUrl" :class="$style.root"/>
<div v-else :class="[$style.root, $style.sky, { [$style.still]: !prefer.s.animation }]">
	<div :class="$style.art" :style="{ backgroundImage: `url(${art})` }"></div>
	<svg :class="$style.sparkles" viewBox="0 0 1280 800" preserveAspectRatio="xMidYMid slice" aria-hidden="true">
		<path :class="$style.sparkle" style="--d: 5s;" d="M640 70 l2 8 8 2 -8 2 -2 8 -2 -8 -8 -2 8 -2z"/>
		<path :class="[$style.sparkle, $style.lav]" style="--d: 6.5s; --dl: -2s;" d="M1180 160 l1.5 6 6 1.5 -6 1.5 -1.5 6 -1.5 -6 -6 -1.5 6 -1.5z"/>
		<path :class="[$style.sparkle, $style.warm]" style="--d: 5.8s; --dl: -3.5s;" d="M1010 640 l2 8 8 2 -8 2 -2 8 -2 -8 -8 -2 8 -2z"/>
	</svg>
</div>
</template>

<script lang="ts" setup>
import MkFeaturedPhotos from '@/components/MkFeaturedPhotos.vue';
import art from '/client-assets/elythia-entrance-art.webp';
import { instance } from '@/instance.js';
import { prefer } from '@/preferences.js';
</script>

<style lang="scss" module>
.root {
	position: fixed;
	inset: 0;
	// 固定レイヤがホイール操作を奪い、コンテンツ列以外の上でページをスクロールできなくなるのを防ぐ (issue #17680)
	pointer-events: none;
}

.sky {
	overflow: hidden;
	background: radial-gradient(140% 100% at 50% 110%, #1b1d4a 0%, #0a112e 45%, #060a1f 100%);

	&::before {
		content: "";
		position: absolute;
		inset: 0;
		background-image:
			radial-gradient(1.2px 1.2px at 12% 18%, #fdf1eb 50%, transparent 51%),
			radial-gradient(1px 1px at 27% 62%, #ccc3f7 50%, transparent 51%),
			radial-gradient(1.4px 1.4px at 44% 30%, #fdf1eb 50%, transparent 51%),
			radial-gradient(1px 1px at 58% 78%, #fdf1eb 50%, transparent 51%),
			radial-gradient(1.6px 1.6px at 71% 14%, #fdddcf 50%, transparent 51%),
			radial-gradient(1px 1px at 83% 47%, #ccc3f7 50%, transparent 51%),
			radial-gradient(1.2px 1.2px at 92% 72%, #fdf1eb 50%, transparent 51%),
			radial-gradient(1px 1px at 6% 84%, #fdf1eb 50%, transparent 51%),
			radial-gradient(1px 1px at 36% 91%, #ccc3f7 50%, transparent 51%),
			radial-gradient(1.3px 1.3px at 64% 52%, #fdf1eb 50%, transparent 51%);
		background-size: 420px 420px;
		opacity: 0.9;
	}

	&::after {
		content: "";
		position: absolute;
		inset: 0;
		background:
			radial-gradient(40% 35% at 18% 30%, rgba(166, 155, 251, 0.22), transparent 70%),
			radial-gradient(35% 30% at 85% 85%, rgba(253, 221, 207, 0.12), transparent 70%),
			radial-gradient(30% 25% at 60% 5%, rgba(204, 195, 247, 0.16), transparent 70%);
	}
}

// 図形を描かず、アイコンの絵そのものを大きく薄く重ねる。screen で重ねるので、
// 絵の暗い部分は夜空に消え、光っている部分だけが浮かぶ
.art {
	position: absolute;
	z-index: 1;
	top: 50%;
	left: 0;
	width: min(1000px, 125vh);
	aspect-ratio: 1;
	transform: translate(-26%, -50%);
	background-position: center;
	background-size: cover;
	mix-blend-mode: screen;
	opacity: 0.32;
	mask-image: radial-gradient(closest-side, #000 55%, transparent 100%);
	animation: breathe 9s ease-in-out infinite;

	@media (max-width: 700px) {
		top: 38%;
		left: 50%;
		width: min(800px, 190vw);
		transform: translate(-50%, -50%);
	}
}

.sparkles {
	position: absolute;
	z-index: 1;
	inset: 0;
	width: 100%;
	height: 100%;
}

.sparkle {
	fill: #fdf1eb;
	transform-box: fill-box;
	transform-origin: center;
	animation: twinkle var(--d, 4s) ease-in-out infinite;
	animation-delay: var(--dl, 0s);

	&.lav {
		fill: #ccc3f7;
	}

	&.warm {
		fill: #fdddcf;
	}
}

.still .art,
.still .sparkle {
	animation: none;
}

@media (prefers-reduced-motion: reduce) {
	.art,
	.sparkle {
		animation: none;
	}
}

@keyframes breathe {
	0%, 100% {
		opacity: 0.26;
	}
	50% {
		opacity: 0.32;
	}
}

@keyframes twinkle {
	0%, 100% {
		opacity: 0.25;
		transform: scale(0.6) rotate(0deg);
	}
	50% {
		opacity: 1;
		transform: scale(1) rotate(20deg);
	}
}
</style>
