<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div v-if="hasMounts" :class="$style.root">
	<!--
		Vue コンポーネント形式。ホストのアプリ内で描画されるので、
		provide/inject もテーマも本体と同じものが効く (= 見た目が完全に一致する)。
	-->
	<component
		:is="entry.renderer.component"
		v-for="entry in componentEntries"
		:key="entry.plugin"
		:ctx="ctx ?? {}"
	/>

	<!-- DOM 直描画形式。Vue を使わないプラグイン向け。 -->
	<div v-if="mountEntries.length > 0" ref="rootEl" :class="$style.mounts"></div>
</div>
</template>

<script lang="ts" setup>
import { computed, onMounted, onBeforeUnmount, useTemplateRef } from 'vue';
import { slotMounts } from '@/plugin-api.js';
import type { SlotName, SlotContext, SlotMount } from '@/plugin-api.js';

/*
 * サーバープラグインの描画先 (mk-go #2479)。
 *
 * 2 つの形式を受ける。Misskey のコンポーネントを使いたいプラグインは Vue
 * コンポーネントを、フレームワークに依存したくないプラグインは DOM 直描画を
 * 選ぶ。
 */
const props = defineProps<{
	name: SlotName;
	ctx?: SlotContext;
}>();

const rootEl = useTemplateRef('rootEl');

const entries = computed(() => slotMounts(props.name));
const hasMounts = computed(() => entries.value.length > 0);

const componentEntries = computed(() =>
	entries.value.filter((e): e is { plugin: string; renderer: { component: NonNullable<unknown> } } =>
		typeof e.renderer === 'object' && e.renderer !== null && 'component' in e.renderer),
);
const mountEntries = computed(() =>
	entries.value.filter((e): e is { plugin: string; renderer: SlotMount } => typeof e.renderer === 'function'),
);

const cleanups: (() => void)[] = [];

onMounted(() => {
	const el = rootEl.value;
	if (el == null) return;

	for (const { plugin, renderer } of mountEntries.value) {
		// プラグインごとに専用の container を渡す。同じ要素を共有させると、
		// 片方が innerHTML を書き換えて他方を消してしまう。
		const container = window.document.createElement('div');
		container.dataset.plugin = plugin;
		el.appendChild(container);

		try {
			// **1 つが落ちても他を止めない。** プラグインの不具合でページ全体が
			// 描画されない方が損害が大きい。
			const cleanup = renderer(container, props.ctx ?? {});
			if (typeof cleanup === 'function') cleanups.push(cleanup);
		} catch (err) {
			console.error(`[plugin:${plugin}] slot ${props.name} の描画に失敗しました`, err);
		}
	}
});

onBeforeUnmount(() => {
	for (const cleanup of cleanups) {
		try {
			cleanup();
		} catch (err) {
			console.error('[plugin] 後片付けに失敗しました', err);
		}
	}
});
</script>

<style lang="scss" module>
.root {
	display: contents;
}

.mounts {
	display: contents;
}
</style>
