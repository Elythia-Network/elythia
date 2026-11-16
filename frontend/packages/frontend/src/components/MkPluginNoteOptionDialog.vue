<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkModalWindow
	ref="dialog"
	:width="450"
	:withOkButton="true"
	@click="cancel()"
	@ok="ok()"
	@close="cancel()"
	@closed="emit('closed')"
>
	<template #header>{{ title }}</template>

	<div class="_spacer _gaps" style="--MI_SPACER-min: 20px; --MI_SPACER-max: 32px;">
		<component :is="component" v-model="value"/>
		<MkButton danger data-testid="plugin-note-option-clear" @click="clear()"><i class="ti ti-x"></i> {{ i18n.ts.clear }}</MkButton>
	</div>
</MkModalWindow>
</template>

<script lang="ts" setup>
import { ref, useTemplateRef } from 'vue';
import type { Component } from 'vue';
import MkModalWindow from '@/components/MkModalWindow.vue';
import MkButton from '@/components/MkButton.vue';
import { i18n } from '@/i18n.js';

const props = defineProps<{
	title: string;
	component: Component;
	initialValue: unknown;
}>();

const emit = defineEmits<{
	(ev: 'done', v: { canceled: true } | { canceled: false; value: unknown }): void;
	(ev: 'closed'): void;
}>();

const dialog = useTemplateRef('dialog');

// 編集中の値は写しに持つ。取り消したときに、フォームの値を書き換えないため。
// 値はAPIへJSONで送るものなので、JSONで写す。
const value = ref<unknown>(props.initialValue == null ? null : JSON.parse(JSON.stringify(props.initialValue)));

let settled = false;

function settle(result: { canceled: true } | { canceled: false; value: unknown }) {
	if (settled) return;
	settled = true;
	emit('done', result);
	dialog.value?.close();
}

function ok() {
	settle({ canceled: false, value: value.value });
}

function clear() {
	settle({ canceled: false, value: null });
}

function cancel() {
	settle({ canceled: true });
}
</script>
