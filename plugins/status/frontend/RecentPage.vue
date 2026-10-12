<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div style="padding: 16px; max-width: 700px; margin: 0 auto;">
	<div v-if="items == null"><MkLoading/></div>
	<div v-else-if="items.length === 0" style="opacity: 0.7;">
		まだ誰もステータスを設定していません。
	</div>
	<div v-else class="_gaps_s">
		<div v-for="(it, i) in items" :key="i" :class="$style.row">
			<span :class="$style.name">@{{ it.username }}</span>
			<span :class="$style.text">{{ it.text }}</span>
		</div>
	</div>
</div>
</template>

<script lang="ts" setup>
import { ref, onMounted } from 'vue';
import { MkLoading } from '@/plugin-api.js';
import { api } from './api.js';

type Item = { userId: string; text: string; username: string; updatedAt: string };

const items = ref<Item[] | null>(null);

onMounted(async () => {
	try {
		const res = await api<{ items: Item[] }>('recent');
		items.value = res.items;
	} catch (err) {
		console.error('[plugin:status] 一覧の取得に失敗しました', err);
		items.value = [];
	}
});
</script>

<style lang="scss" module>
.row {
	display: flex;
	gap: 10px;
	padding: 8px 12px;
	border-radius: var(--MI-radius-sm, 8px);
	background: var(--MI_THEME-panel);
}

.name {
	flex-shrink: 0;
	opacity: 0.7;
}

.text {
	min-width: 0;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}
</style>
