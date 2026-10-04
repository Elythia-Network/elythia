<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: 承認制の登録の審査画面 (#2554 / #2555)。

	申請は「他の Misskey サーバーのアカウント」を連絡先として作られる。承認しても
	この時点では user 行を作らず、申請者が登録ページに戻ってきたときに初めて作る。

	有効・無効の切り替えはコントロールパネルのモデレーションにある (#2557)。
	設定と審査を 1 画面に混ぜると、審査のたびに設定が目に入って誤操作しやすい。
-->
<template>
<PageWithHeader :actions="headerActions" :tabs="headerTabs">
	<div class="_spacer" style="--MI_SPACER-w: 800px; --MI_SPACER-min: 16px; --MI_SPACER-max: 32px;">
		<div class="_gaps_m">
			<MkSelect v-model="filter" :items="filterDef">
				<template #label>表示</template>
			</MkSelect>

			<MkInfo v-if="applications.length === 0">
				{{ filter === 'pending' ? '審査待ちの申請はありません。' : '該当する申請はありません。' }}
			</MkInfo>

			<div v-else class="_gaps_s">
				<MkFolder v-for="app in applications" :key="app.id" :defaultOpen="filter === 'pending'">
					<template #icon><i class="ti ti-user-plus"></i></template>
					<template #label><MkTime :time="app.createdAt" mode="detail"/></template>
					<template #suffix>{{ statusLabel(app.status) }}</template>

					<div class="_gaps_s">
						<MkKeyValue oneline>
							<template #key>申請日時</template>
							<template #value><MkTime :time="app.createdAt" mode="detail"/></template>
						</MkKeyValue>
						<MkKeyValue oneline>
							<template #key>期限</template>
							<template #value><MkTime :time="app.expiresAt" mode="detail"/></template>
						</MkKeyValue>
						<MkKeyValue v-if="app.processedAt != null" oneline>
							<template #key>審査日時</template>
							<template #value><MkTime :time="app.processedAt" mode="detail"/></template>
						</MkKeyValue>
						<MkKeyValue v-for="(answer, i) in app.answers" :key="i">
							<template #key>{{ answer.label }}</template>
							<template #value><div :class="$style.reason">{{ answer.value || '(未回答)' }}</div></template>
						</MkKeyValue>

						<div v-if="app.status === 'pending'" :class="$style.actions">
							<MkButton primary inline @click="approve(app)"><i class="ti ti-check"></i> 承認</MkButton>
							<MkButton danger inline @click="reject(app)"><i class="ti ti-x"></i> 却下</MkButton>
						</div>
					</div>
				</MkFolder>
			</div>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { computed, ref, watch } from 'vue';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkSelect from '@/components/MkSelect.vue';
import { useMkSelect } from '@/composables/use-mkselect.js';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { definePage } from '@/page.js';

type Answer = {
	label: string;
	value: string;
};

type SignupApplication = {
	id: string;
	status: 'pending' | 'approved' | 'rejected' | 'expired' | 'completed';
	// 回答は提出時のラベル付き。**定義を変えても既存申請が読める** (#2570)。
	answers: Answer[];
	createdAt: string;
	updatedAt: string;
	expiresAt: string;
	processedById: string | null;
	processedAt: string | null;
	usedById: string | null;
};

// mk-go 独自のエンドポイントなので misskey-js の型集合には無い。
// server-plugins.vue と同じ理由の cast。
function api<T>(endpoint: string, params: Record<string, unknown> = {}): Promise<T> {
	return misskeyApi(endpoint as never, params as never) as unknown as Promise<T>;
}

const applications = ref<SignupApplication[]>([]);
const {
	model: filter,
	def: filterDef,
} = useMkSelect({
	items: [
		{ label: '審査待ち', value: 'pending' },
		{ label: '承認済み（未登録）', value: 'approved' },
		{ label: '処理済み', value: 'processed' },
		{ label: 'すべて', value: 'all' },
	],
	initialValue: 'pending',
});

const statusLabels: Record<SignupApplication['status'], string> = {
	pending: '審査待ち',
	approved: '承認済み（未登録）',
	rejected: '却下',
	expired: '期限切れ',
	completed: '登録済み',
};

function statusLabel(status: SignupApplication['status']): string {
	return statusLabels[status];
}

async function refresh() {
	const res = await api<{ applications: SignupApplication[]; count: number }>(
		'admin/signup-application/list', { filter: filter.value, limit: 100 });
	applications.value = res.applications;
}

async function approve(app: SignupApplication) {
	const { canceled } = await os.confirm({
		type: 'question',
		text: 'この申請を承認しますか？',
	});
	if (canceled) return;
	await os.apiWithDialog('admin/signup-application/approve' as never, { applicationId: app.id } as never);
	await refresh();
}

async function reject(app: SignupApplication) {
	const { canceled } = await os.confirm({
		type: 'warning',
		text: 'この申請を却下しますか？',
	});
	if (canceled) return;
	await os.apiWithDialog('admin/signup-application/reject' as never, { applicationId: app.id } as never);
	await refresh();
}

watch(filter, refresh);

await refresh();

const headerActions = computed(() => [{
	icon: 'ti ti-refresh',
	text: '再読み込み',
	handler: refresh,
}]);

const headerTabs = computed(() => []);

definePage(() => ({
	title: '登録申請',
	icon: 'ti ti-user-plus',
}));
</script>

<style lang="scss" module>
.actions {
	display: flex;
	gap: 8px;
	flex-wrap: wrap;
}

/* 申請理由は自由入力。長い行や改行をそのまま出すと横に溢れる。 */
.reason {
	white-space: pre-wrap;
	overflow-wrap: anywhere;
}
</style>
