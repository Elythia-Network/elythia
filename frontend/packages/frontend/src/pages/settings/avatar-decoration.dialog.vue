<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkModalWindow
	ref="dialog"
	:width="400"
	:height="560"
	@close="cancel"
	@closed="emit('closed')"
>
	<template #header>{{ i18n.ts.avatarDecorations }}</template>

	<div>
		<div class="_spacer" style="--MI_SPACER-min: 20px; --MI_SPACER-max: 28px;">
			<!--
				**プレビューはスクロールしても上に残す (#3188)。** スライダーを動かして
				結果を見る画面なので、スクロールで流れると調整中に結果が見えない。
				高さを上げても、窓の低い PC やスマートフォンのドロワー (高さ固定) では
				スクロールが残る。
			-->
			<div :class="$style.preview">
				<div :class="$style.name">{{ decoration.name }}</div>
				<MkAvatar style="width: 64px; height: 64px; margin-bottom: 20px;" :user="$i" :decorations="decorationsForPreview" forceShowDecoration/>
			</div>
			<div class="_gaps_s">
				<!--
					大きさ (#2975、mk-go 独自)。**拡大は無い** — `.decoration` は
					既にアバターの 2 倍の枠に描かれるので、1 が upstream と同じ
					最大サイズになる。余白を持たないカスタム絵文字はそのままだと
					アイコンを覆うため、ここで縮める。
				-->
				<MkRange v-model="scale" continuousUpdate :min="0.1" :max="1" :step="0.05" :textConverter="(v) => `${Math.round(v * 100)}%`">
					<template #label>{{ i18n.ts._mkgoAvatarDecoration.size }}</template>
				</MkRange>
				<MkRange v-model="angle" continuousUpdate :min="-0.5" :max="0.5" :step="0.025" :textConverter="(v) => `${Math.floor(v * 360)}°`">
					<template #label>{{ i18n.ts.angle }}</template>
				</MkRange>
				<MkRange v-model="offsetX" continuousUpdate :min="-0.25" :max="0.25" :step="0.025" :textConverter="(v) => `${Math.floor(v * 100)}%`">
					<template #label>X {{ i18n.ts.position }}</template>
				</MkRange>
				<MkRange v-model="offsetY" continuousUpdate :min="-0.25" :max="0.25" :step="0.025" :textConverter="(v) => `${Math.floor(v * 100)}%`">
					<template #label>Y {{ i18n.ts.position }}</template>
				</MkRange>
				<MkSwitch v-model="flipH">
					<template #label>{{ i18n.ts.flip }}</template>
				</MkSwitch>
			</div>
		</div>

		<div :class="$style.footer" class="_buttonsCenter">
			<MkButton v-if="usingIndex != null" primary rounded @click="update"><i class="ti ti-check"></i> {{ i18n.ts.update }}</MkButton>
			<MkButton v-if="usingIndex != null" rounded @click="detach"><i class="ti ti-x"></i> {{ i18n.ts.detach }}</MkButton>
			<MkButton v-else :disabled="exceeded || locked" primary rounded @click="attach"><i class="ti ti-check"></i> {{ i18n.ts.attach }}</MkButton>
		</div>
	</div>
</MkModalWindow>
</template>

<script lang="ts" setup>
import { useTemplateRef, ref, computed } from 'vue';
import MkButton from '@/components/MkButton.vue';
import MkModalWindow from '@/components/MkModalWindow.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import { i18n } from '@/i18n.js';
import MkRange from '@/components/MkRange.vue';
import { ensureSignin } from '@/i.js';

const $i = ensureSignin();

const props = defineProps<{
	usingIndex: number | null;
	/**
	 * 新規装着のときの大きさの初期値 (#2975、mk-go 独自)。カスタム絵文字は
	 * 余白を持たないので、呼び出し側が小さめの値を渡す。省略で 1。
	 */
	defaultScale?: number;
	decoration: {
		id: string;
		url: string;
		name: string;
		roleIdsThatCanBeUsedThisDecoration: string[];
	};
}>();

const emit = defineEmits<{
	(ev: 'closed'): void;
	(ev: 'attach', payload: {
		angle: number;
		flipH: boolean;
		offsetX: number;
		offsetY: number;
		scale: number;
	}): void;
	(ev: 'update', payload: {
		angle: number;
		flipH: boolean;
		offsetX: number;
		offsetY: number;
		scale: number;
	}): void;
	(ev: 'detach'): void;
}>();

const dialog = useTemplateRef('dialog');
const exceeded = computed(() => ($i.policies.avatarDecorationLimit - $i.avatarDecorations.length) <= 0);
const locked = computed(() => props.decoration.roleIdsThatCanBeUsedThisDecoration.length > 0 && !$i.roles.some(r => props.decoration.roleIdsThatCanBeUsedThisDecoration.includes(r.id)));
const angle = ref((props.usingIndex != null ? $i.avatarDecorations[props.usingIndex].angle : null) ?? 0);
const flipH = ref((props.usingIndex != null ? $i.avatarDecorations[props.usingIndex].flipH : null) ?? false);
const offsetX = ref((props.usingIndex != null ? $i.avatarDecorations[props.usingIndex].offsetX : null) ?? 0);
const offsetY = ref((props.usingIndex != null ? $i.avatarDecorations[props.usingIndex].offsetY : null) ?? 0);
// mk-go 独自 (#2975)。autogen 型に無いのでキャストで読む。装着済みなら保存値、
// 新規なら呼び出し側の既定 (絵文字は小さめ)、どちらも無ければ 1。
const scale = ref(
	(props.usingIndex != null
		? ($i.avatarDecorations[props.usingIndex] as { scale?: number }).scale
		: null) ?? props.defaultScale ?? 1,
);

const decorationsForPreview = computed(() => {
	const decoration = {
		id: props.decoration.id,
		url: props.decoration.url,
		angle: angle.value,
		flipH: flipH.value,
		offsetX: offsetX.value,
		offsetY: offsetY.value,
		scale: scale.value,
		blink: true,
	};
	const decorations = [...$i.avatarDecorations];
	if (props.usingIndex != null) {
		decorations[props.usingIndex] = decoration;
	} else {
		decorations.push(decoration);
	}
	return decorations;
});

function cancel() {
	dialog.value?.close();
}

async function update() {
	emit('update', {
		angle: angle.value,
		flipH: flipH.value,
		offsetX: offsetX.value,
		offsetY: offsetY.value,
		scale: scale.value,
	});
	dialog.value?.close();
}

async function attach() {
	emit('attach', {
		angle: angle.value,
		flipH: flipH.value,
		offsetX: offsetX.value,
		offsetY: offsetY.value,
		scale: scale.value,
	});
	dialog.value?.close();
}

async function detach() {
	emit('detach');
	dialog.value?.close();
}
</script>

<style lang="scss" module>
.preview {
	position: sticky;
	top: 0;
	// 下を流れるスライダーより前に出す。`.name` の z-index (10) より上。
	z-index: 11;
	text-align: center;
	// 塗らないとスライダーが透ける。スクロール領域 (MkModalWindow の .body) と同じ色。
	background: var(--MI_THEME-bg);
}

.name {
	position: relative;
	z-index: 10;
	font-weight: bold;
	margin-bottom: 28px;
}

.footer {
	position: sticky;
	bottom: 0;
	left: 0;
	padding: 12px;
	border-top: solid 0.5px var(--MI_THEME-divider);
	-webkit-backdrop-filter: var(--MI-blur, blur(15px));
	backdrop-filter: var(--MI-blur, blur(15px));
}
</style>
