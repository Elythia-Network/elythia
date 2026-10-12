<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkFolder>
	<template #label>ステータス</template>
	<template #suffix>{{ current ?? '未設定' }}</template>

	<div class="_gaps_m">
		<MkInput v-model="draft" type="text">
			<template #label>ひとこと</template>
			<template #caption>
				{{ maxLength }}文字まで。プロフィールに表示されます。空にすると消えます。
			</template>
		</MkInput>

		<div>
			<div :class="$style.label">表示期間</div>
			<div class="_buttons">
				<MkButton
					v-for="d in options"
					:key="d.value"
					:primary="duration === d.value"
					small
					rounded
					@click="duration = d.value"
				>{{ d.label }}</MkButton>
			</div>
		</div>

		<div class="_buttons">
			<MkButton primary :disabled="saving" @click="save">
				<template v-if="saving"><MkLoading :em="true"/></template>
				<template v-else>保存</template>
			</MkButton>
			<MkButton :disabled="saving || current == null" @click="clear">消す</MkButton>
		</div>

		<!--
			結果はここに出す。バックエンドが返した理由 (文字数超過など) を
			そのまま見せる — 利用者が直せるものなので。
		-->
		<div v-if="message" :class="failed ? $style.error : $style.ok">{{ message }}</div>
	</div>
</MkFolder>
</template>

<script lang="ts" setup>
import { ref, onMounted } from 'vue';
import { MkInput, MkButton, MkFolder, MkLoading } from '@/plugin-api.js';
import { api } from './api.js';
import type { Duration, MeResponse } from './api.js';

const options: { value: Duration; label: string }[] = [
	{ value: '1h', label: '1時間' },
	{ value: '1d', label: '1日' },
	{ value: '1w', label: '1週間' },
	{ value: '', label: '無期限' },
];

const current = ref<string | null>(null);
const draft = ref('');
const duration = ref<Duration>('1d');
const maxLength = ref(30);
const saving = ref(false);
const message = ref('');
const failed = ref(false);

onMounted(async () => {
	try {
		const me = await api<MeResponse>('me');
		current.value = me.text;
		draft.value = me.text ?? '';
		maxLength.value = me.maxLength;
	} catch (err) {
		// 現在値が読めなくても入力欄は使えるままにする。
		console.error('[plugin:status] 現在値を取得できませんでした', err);
	}
});

async function submit(text: string): Promise<void> {
	saving.value = true;
	message.value = '';
	try {
		const res = await api<MeResponse>('me/set', { text, duration: duration.value });
		current.value = res.text;
		draft.value = res.text ?? '';
		failed.value = false;
		message.value = res.text == null ? '消しました' : '保存しました';
	} catch (err) {
		failed.value = true;
		message.value = (err as { message?: string } | null)?.message ?? '保存に失敗しました';
	} finally {
		saving.value = false;
	}
}

function save(): void {
	void submit(draft.value);
}

function clear(): void {
	void submit('');
}
</script>

<style lang="scss" module>
.label {
	font-size: 0.85em;
	opacity: 0.8;
	margin-bottom: 6px;
}

.ok {
	color: var(--MI_THEME-success);
	font-size: 0.9em;
}

.error {
	color: var(--MI_THEME-error);
	font-size: 0.9em;
}
</style>
