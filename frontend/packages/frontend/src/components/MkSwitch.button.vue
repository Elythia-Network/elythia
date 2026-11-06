<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<span
	v-tooltip="checked ? i18n.ts.itsOn : i18n.ts.itsOff"
	:class="{
		[$style.button]: true,
		[$style.buttonChecked]: checked,
		[$style.buttonDisabled]: props.disabled
	}"
	data-testid="switch-toggle"
	:data-checked="isChecked"
	@click.prevent.stop="toggle"
>
	<div :class="{ [$style.knob]: true, [$style.knobChecked]: checked }"></div>
</span>
</template>

<script lang="ts" setup>
import { computed, toRefs, unref } from 'vue';
import type { Ref } from 'vue';
import { i18n } from '@/i18n.js';

const props = withDefaults(defineProps<{
	checked: boolean | Ref<boolean>;
	disabled?: boolean | Ref<boolean>;
}>(), {
	disabled: false,
});

const emit = defineEmits<{
	(ev: 'toggle'): void;
}>();

const checked = toRefs(props).checked;
// オンかオフかを、ハッシュでない属性で出す (Playwright の設定の表が読む、#4056)。
// role="switch" と aria-checked は使わない。MkMenu の menuitemcheckbox の中に入れ子になり、
// 隣の非表示の checkbox と合わせて、支援技術から部品が 2 つに見えるため。
// checked は値か Ref のどちらでも来るので、真偽値にそろえる
const isChecked = computed(() => unref(checked.value) === true);
const toggle = () => {
	emit('toggle');
};
</script>

<style lang="scss" module>
.button {
	--height: 21px;

	position: relative;
	display: inline-flex;
	flex-shrink: 0;
	margin: 0;
	box-sizing: border-box;
	width: calc(var(--height) * 1.6);
	height: calc(var(--height) + 2px); // 枠線
	outline: none;
	background: var(--MI_THEME-switchOffBg);
	background-clip: content-box;
	border: solid 1px var(--MI_THEME-switchOffBg);
	border-radius: 999px;
	cursor: pointer;
	transition: inherit;
	user-select: none;
}

.buttonChecked {
	background-color: var(--MI_THEME-switchOnBg) !important;
	border-color: var(--MI_THEME-switchOnBg) !important;
}

.buttonDisabled {
	cursor: not-allowed;
}

.knob {
	position: absolute;
	box-sizing: border-box;
	top: 3px;
	width: calc(var(--height) - 6px);
	height: calc(var(--height) - 6px);
	border-radius: 999px;
	transition: all 0.2s ease;

	&:not(.knobChecked) {
		left: 3px;
		background: var(--MI_THEME-switchOffFg);
	}
}

.knobChecked {
	left: calc(calc(100% - var(--height)) + 3px);
	background: var(--MI_THEME-switchOnFg);
}
</style>
