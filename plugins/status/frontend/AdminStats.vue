<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div class="_gaps_m" style="padding: 16px;">
	<div v-if="error">{{ error }}</div>
	<template v-else-if="stats">
		<div>設定されているステータス: <b>{{ stats.total }}</b></div>
		<div>うち期限付き: <b>{{ stats.expiring }}</b></div>
	</template>
	<MkLoading v-else/>
</div>
</template>

<script lang="ts" setup>
import { ref, onMounted } from 'vue';
import { MkLoading } from '@/plugin-api.js';
import { api } from './api.js';

const stats = ref<{ total: number; expiring: number } | null>(null);
const error = ref('');

onMounted(async () => {
	try {
		stats.value = await api<{ total: number; expiring: number }>('admin/stats');
	} catch (err) {
		// 権限が無い場合もここに来る。バックエンドが返した理由を見せる。
		error.value = (err as { message?: string } | null)?.message ?? '取得に失敗しました';
	}
});
</script>
