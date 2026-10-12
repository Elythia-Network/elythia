<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div v-if="status" :class="$style.card">
	<span :class="$style.icon">💬</span>
	<span :class="$style.text">{{ status.text }}</span>
	<span v-if="remaining" :class="$style.remaining">{{ remaining }}</span>
</div>
</template>

<script lang="ts" setup>
import { ref, computed, onMounted } from 'vue';
import { type SlotContext } from '@/plugin-api.js';
import { api } from './api.js';
import type { ShowResponse } from './api.js';

const props = defineProps<{ ctx: SlotContext }>();

type Shown = { text: string; expiresAt: string | null };
const status = ref<Shown | null>(null);

const remaining = computed(() => {
	const at = status.value?.expiresAt;
	if (at == null) return '';

	const ms = new Date(at).getTime() - Date.now();
	if (ms <= 0) return '';
	const hours = Math.floor(ms / 3600_000);
	if (hours >= 24) return `あと ${Math.floor(hours / 24)} 日`;
	if (hours >= 1) return `あと ${hours} 時間`;
	return `あと ${Math.max(1, Math.floor(ms / 60_000))} 分`;
});

onMounted(async () => {
	const user = props.ctx.user;
	if (user == null) return;
	// リモートユーザーには出さない。このインスタンスに保存されたものしか
	// 持っていないので、他所のユーザーでは必ず未設定になる。
	if (user.host != null) return;

	try {
		const res = await api<ShowResponse>('show', { userId: user.id });
		if (res.set) status.value = { text: res.text, expiresAt: res.expiresAt };
	} catch (err) {
		// 表示できないだけで済ませる。プラグインの不具合でプロフィール全体が
		// 壊れてはいけない。
		console.error('[plugin:status] 取得に失敗しました', err);
	}
});
</script>

<style lang="scss" module>
.card {
	display: flex;
	align-items: center;
	gap: 8px;
	margin: 8px 0;
	padding: 8px 12px;
	border-radius: var(--MI-radius-sm, 8px);
	background: var(--MI_THEME-buttonBg);
	font-size: 0.95em;
}

.icon {
	flex-shrink: 0;
}

.text {
	flex: 1;
	min-width: 0;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.remaining {
	flex-shrink: 0;
	font-size: 0.85em;
	opacity: 0.6;
}
</style>
