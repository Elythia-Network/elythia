<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div v-if="meta" :class="$style.root" :style="themeVars">
	<XBackdrop/>
	<div :class="$style.logoWrapper">
		<div :class="$style.poweredBy">Powered by</div>
		<div :class="$style.elythia"><img :src="elythiaIcon" alt="" :class="$style.elythiaIcon"/>Elythia</div>
	</div>
	<div :class="$style.contents">
		<MkVisitorDashboard/>
	</div>
</div>
</template>

<script lang="ts" setup>
import XBackdrop from './welcome.elythia-backdrop.vue';
import elythiaIcon from '/client-assets/elythia-icon.png';
import MkVisitorDashboard from '@/components/MkVisitorDashboard.vue';
import { instance as meta } from '@/instance.js';
import { prefer } from '@/preferences.js';
import { entranceThemeVars } from '@/utility/elythia-entrance.js';

// Elythia: エントランスは利用者のテーマに依らず夜空のガラスにする
const themeVars = entranceThemeVars(prefer.s.useBlurEffect);
</script>

<style lang="scss" module>
.root {
	height: 100cqh;
	overflow: auto;
	overscroll-behavior: contain;
	// 文字色は html で var(--MI_THEME-fg) を解決した値が継承されてくるので、部分木で
	// 変数を上書きしただけでは変わらない。ここで宣言し直す
	color: var(--MI_THEME-fg);
	accent-color: var(--MI_THEME-accent);
}

.logoWrapper {
	position: fixed;
	top: 36px;
	left: 36px;
	flex: auto;
	color: #fff;
	user-select: none;
	pointer-events: none;
}

.poweredBy {
	margin-bottom: 3px;
	font-size: 10px;
	letter-spacing: 0.08em;
	opacity: 0.75;
}

.elythia {
	display: flex;
	align-items: center;
	gap: 7px;
	font-size: 15px;
	font-weight: 500;
	letter-spacing: 0.14em;
	line-height: 1;
}

.elythiaIcon {
	width: 22px;
	height: 22px;
	border-radius: 24%;
	box-shadow: 0 0 0 1px rgba(204, 195, 247, 0.25), 0 0 12px rgba(166, 155, 251, 0.45);
}

.contents {
	position: relative;
	width: min(430px, calc(100% - 32px));
	margin: auto;
	padding: 100px 0 100px 0;
}
</style>
