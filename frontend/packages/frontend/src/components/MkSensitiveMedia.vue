<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div :class="[$style.root, (isSensitive && highlight) && $style.sensitive]" data-testid="sensitive-media">
	<button v-if="hide" type="button" :class="[$style.hidden, type === 'image' ? $style.hiddenImage : $style.hiddenPlayer]" data-testid="sensitive-media-hidden" @click.stop="reveal">
		<span :class="$style.hiddenText">
			<b v-if="isSensitive" style="display: block;"><i class="ti ti-eye-exclamation"></i> {{ i18n.ts.sensitive }}{{ dataSaverMedia ? ` (${kindLabel}${size ? ' ' + bytes(size) : ''})` : '' }}</b>
			<b v-else style="display: block;"><i :class="kindIcon"></i> {{ dataSaverMedia && size ? bytes(size) : kindLabel }}</b>
			<span style="display: block;">{{ i18n.ts.clickToShow }}</span>
		</span>
	</button>
	<template v-else>
		<slot :hide="hideAgain"></slot>
		<button v-if="controls" type="button" :class="$style.hideButton" class="_button" :aria-label="i18n.ts.hide" data-testid="sensitive-media-hide" @click.stop="hideAgain"><i class="ti ti-eye-off" style="vertical-align: middle;" aria-hidden="true"></i></button>
	</template>
</div>
</template>

<script lang="ts" setup>
// プラグインが外部から取ったメディアを、本体のメディア表示と同じ規則で隠す (Elythia #4191)。
// 隠している間は slot を描画しない。中身を描画したまま覆うと、表示する前から外部へ取りに行き、
// データセーバーも効かなくなるため。
import { computed, ref, watch } from 'vue';
import bytes from '@/filters/bytes.js';
import { i18n } from '@/i18n.js';
import { prefer } from '@/preferences.js';
import { canRevealSensitiveMedia, shouldHideSensitiveMediaByDefault } from '@/utility/sensitive-file.js';

const props = withDefaults(defineProps<{
	isSensitive: boolean;
	type?: 'image' | 'video' | 'audio';
	size?: number | null;
	controls?: boolean;
}>(), {
	type: 'image',
	size: null,
	controls: true,
});

const hide = ref(true);

// prefer.s は reactive ではない。設定の変化を拾うには prefer.r を読む。
const nsfw = computed(() => prefer.r.nsfw.value);
const dataSaverMedia = computed(() => prefer.r.dataSaver.value.media);
const highlight = computed(() => prefer.r.highlightSensitiveMedia.value);

// 確認を待つ間に判定や設定が変わったら、その確認の結果では表示しない。
let generation = 0;
let confirming = false;

const kindLabel = computed(() => props.type === 'video' ? i18n.ts.video : props.type === 'audio' ? i18n.ts.audio : i18n.ts.image);
const kindIcon = computed(() => props.type === 'video' ? 'ti ti-movie' : props.type === 'audio' ? 'ti ti-music' : 'ti ti-photo');

// 判定か設定が変わったら、表示した後でも既定の状態へ戻す (MkMediaImage がファイルの変化で戻すのと同じ)。
// rewriteRawState は prefer.s を先に書き換えるので、ここで prefer.s を読む判定にも新しい値が見える。
watch(() => [props.isSensitive, nsfw.value, dataSaverMedia.value] as const, () => {
	generation++;
	hide.value = shouldHideSensitiveMediaByDefault(props.isSensitive);
}, { immediate: true });

async function reveal(): Promise<void> {
	if (confirming) return;
	confirming = true;
	const startedAt = generation;
	let ok: boolean;
	try {
		ok = await canRevealSensitiveMedia(props.isSensitive);
	} finally {
		confirming = false;
	}
	if (!ok || startedAt !== generation) return;
	hide.value = false;
}

function hideAgain(): void {
	hide.value = true;
}
</script>

<style lang="scss" module>
.root {
	position: relative;
	width: 100%;
	height: 100%;
	overflow: clip;
}

.sensitive {
	&::after {
		content: "";
		position: absolute;
		top: 0;
		left: 0;
		width: 100%;
		height: 100%;
		pointer-events: none;
		border-radius: inherit;
		box-shadow: inset 0 0 0 4px var(--MI_THEME-warn);
	}
}

.hidden {
	display: flex;
	align-items: center;
	justify-content: center;
	width: 100%;
	height: 100%;
	padding: 12px 0;
	border: none;
	outline: none;
	font: inherit;
	color: inherit;
	cursor: pointer;

	&:focus-visible {
		outline: 2px solid var(--MI_THEME-focus);
		outline-offset: -2px;
	}
}

.hiddenImage {
	background: #888;
}

.hiddenPlayer {
	background: #000;
}

.hiddenText {
	text-align: center;
	font-size: 0.8em;
	color: #fff;
}

.hideButton {
	display: block;
	position: absolute;
	top: 0;
	right: 0;
	z-index: 1;
	width: 28px;
	height: 28px;
	border-radius: 0 8px 0 8px;
	background-color: rgba(0, 0, 0, 0.3);
	-webkit-backdrop-filter: var(--MI-blur, blur(15px));
	backdrop-filter: var(--MI-blur, blur(15px));
	color: #fff;
	font-size: 0.8em;
	text-align: center;
}
</style>
