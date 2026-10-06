<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div v-if="meta" :class="$style.root" :style="themeVars">
	<XBackdrop/>
	<XTimeline :class="$style.tl"/>
	<div :class="$style.logoWrapper">
		<div :class="$style.poweredBy">Powered by</div>
		<div :class="$style.elythia"><img :src="elythiaIcon" alt="" :class="$style.elythiaIcon"/>Elythia</div>
	</div>
	<div :class="$style.contents">
		<MkVisitorDashboard/>
	</div>
	<div v-if="instances && instances.length > 0" :class="$style.federation">
		<MkMarqueeText :duration="40">
			<MkA v-for="instance in instances" :key="instance.id" :class="$style.federationInstance" :to="`/instance-info/${instance.host}`" behavior="window">
				<!--<MkInstanceCardMini :instance="instance"/>-->
				<img v-if="instance.iconUrl" :class="$style.federationInstanceIcon" :src="getInstanceIcon(instance)" alt=""/>
				<span class="_monospace">{{ instance.host }}</span>
			</MkA>
		</MkMarqueeText>
	</div>
</div>
</template>

<script lang="ts" setup>
import { ref } from 'vue';
import * as Misskey from 'misskey-js';
import XTimeline from './welcome.timeline.vue';
import MkMarqueeText from '@/components/MkMarqueeText.vue';
import XBackdrop from './welcome.elythia-backdrop.vue';
import elythiaIcon from '/client-assets/elythia-icon.png';
import { misskeyApiGet } from '@/utility/misskey-api.js';
import MkVisitorDashboard from '@/components/MkVisitorDashboard.vue';
import { getProxiedImageUrl } from '@/utility/media-proxy.js';
import { instance as meta } from '@/instance.js';
import { prefer } from '@/preferences.js';
import { entranceThemeVars } from '@/utility/elythia-entrance.js';

// Elythia: エントランスは利用者のテーマに依らず夜空のガラスにする
const themeVars = entranceThemeVars(prefer.s.useBlurEffect);

const instances = ref<Misskey.entities.FederationInstance[]>();

function getInstanceIcon(instance: Misskey.entities.FederationInstance): string {
	if (!instance.iconUrl) {
		return '';
	}

	return getProxiedImageUrl(instance.iconUrl, 'preview');
}

misskeyApiGet('federation/instances', {
	sort: '+pubSub',
	limit: 20,
	blocked: false,
}).then(_instances => {
	instances.value = _instances;
});
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

.tl {
	// mask-image がぼかしの基準を切るので、流れるノートにぼかしは効かない。
	// ぼかしをやめ、絵が透けて読みにくくならない濃さにする
	--ELYTHIA-panelBackdrop: none;
	--MI_THEME-panel: rgba(18, 22, 56, 0.92);
	position: fixed;
	top: 0;
	bottom: 0;
	right: 64px;
	margin: auto;
	padding: 128px 0;
	width: 500px;
	height: calc(100% - 256px);
	overflow: hidden;
	-webkit-mask-image: linear-gradient(0deg, rgba(0,0,0,0) 0%, rgba(0,0,0,1) 128px, rgba(0,0,0,1) calc(100% - 128px), rgba(0,0,0,0) 100%);
	mask-image: linear-gradient(0deg, rgba(0,0,0,0) 0%, rgba(0,0,0,1) 128px, rgba(0,0,0,1) calc(100% - 128px), rgba(0,0,0,0) 100%);

	@media (max-width: 1200px) {
		display: none;
	}
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
	margin-left: 128px;
	padding: 100px 0 100px 0;

	@media (max-width: 1200px) {
		margin: auto;
	}
}

.federation {
	position: fixed;
	bottom: 16px;
	left: 0;
	right: 0;
	margin: auto;
	background: color(from var(--MI_THEME-panel) srgb r g b / 0.5);
	-webkit-backdrop-filter: var(--MI-blur, blur(15px));
	backdrop-filter: var(--MI-blur, blur(15px));
	border-radius: 999px;
	overflow: clip;
	width: 800px;
	padding: 8px 0;

	@media (max-width: 900px) {
		display: none;
	}
}

.federationInstance {
	display: inline-flex;
	align-items: center;
	vertical-align: bottom;
	padding: 6px 12px 6px 6px;
	margin: 0 10px 0 0;
	background: var(--MI_THEME-panel);
	border-radius: 999px;
}

.federationInstanceIcon {
	display: inline-block;
	width: 20px;
	height: 20px;
	margin-right: 5px;
	border-radius: 999px;
}
</style>
