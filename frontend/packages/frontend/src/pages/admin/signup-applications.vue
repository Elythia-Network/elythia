<!--
SPDX-FileCopyrightText: Elythia-Network
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
				<template #label>{{ i18n.ts._mkgoSignupApplication._admin.show }}</template>
			</MkSelect>

			<MkInfo v-if="applications.length === 0">
				{{ filter === 'pending' ? i18n.ts._mkgoSignupApplication._admin.noPending : i18n.ts._mkgoSignupApplication._admin.noMatching }}
			</MkInfo>

			<div v-else class="_gaps_s">
				<MkFolder v-for="app in applications" :key="app.id" :defaultOpen="filter === 'pending'" data-testid="signup-application-item" :data-application-id="app.id">
					<template #icon><i class="ti ti-user-plus"></i></template>
					<template #label><MkTime :time="app.createdAt" mode="detail"/></template>
					<template #suffix>{{ statusLabel(app.status) }}</template>

					<div class="_gaps_s">
						<MkKeyValue oneline>
							<template #key>{{ i18n.ts._mkgoSignupApplication._admin.appliedAt }}</template>
							<template #value><MkTime :time="app.createdAt" mode="detail"/></template>
						</MkKeyValue>
						<MkKeyValue oneline>
							<template #key>{{ i18n.ts._mkgoSignupApplication._admin.expiresAt }}</template>
							<template #value><MkTime :time="app.expiresAt" mode="detail"/></template>
						</MkKeyValue>
						<MkKeyValue v-if="app.processedAt != null" oneline>
							<template #key>{{ i18n.ts._mkgoSignupApplication._admin.processedAt }}</template>
							<template #value><MkTime :time="app.processedAt" mode="detail"/></template>
						</MkKeyValue>
						<MkKeyValue v-for="(answer, i) in app.answers" :key="i">
							<template #key>{{ answer.label }}</template>
							<template #value><div :class="$style.reason">{{ answer.value || i18n.ts._mkgoSignupApplication._admin.unanswered }}</div></template>
						</MkKeyValue>

						<div v-if="app.status === 'pending'" :class="$style.actions">
							<MkButton primary inline data-testid="signup-application-approve" @click="approve(app)"><i class="ti ti-check"></i> {{ i18n.ts._mkgoSignupApplication._admin.approve }}</MkButton>
							<MkButton danger inline data-testid="signup-application-reject" @click="reject(app)"><i class="ti ti-x"></i> {{ i18n.ts._mkgoSignupApplication._admin.reject }}</MkButton>
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
import type * as Elythia from 'elythia-js';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkSelect from '@/components/MkSelect.vue';
import { useMkSelect } from '@/composables/use-mkselect.js';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { definePage } from '@/page.js';
import { i18n } from '@/i18n.js';

type SignupApplication = Elythia.AdminSignupApplication;

const applications = ref<SignupApplication[]>([]);
const {
	model: filter,
	def: filterDef,
} = useMkSelect({
	items: [
		{ label: i18n.ts._mkgoSignupApplication._admin.statusPending, value: 'pending' },
		{ label: i18n.ts._mkgoSignupApplication._admin.statusApproved, value: 'approved' },
		{ label: i18n.ts._mkgoSignupApplication._admin.filterProcessed, value: 'processed' },
		{ label: i18n.ts._mkgoSignupApplication._admin.filterAll, value: 'all' },
	],
	initialValue: 'pending',
});

const statusLabels: Record<SignupApplication['status'], string> = {
	pending: i18n.ts._mkgoSignupApplication._admin.statusPending,
	approved: i18n.ts._mkgoSignupApplication._admin.statusApproved,
	rejected: i18n.ts._mkgoSignupApplication._admin.statusRejected,
	expired: i18n.ts._mkgoSignupApplication._admin.statusExpired,
	completed: i18n.ts._mkgoSignupApplication._admin.statusCompleted,
};

function statusLabel(status: SignupApplication['status']): string {
	return statusLabels[status];
}

async function refresh() {
	const res = await misskeyApi(
		'admin/signup-application/list', { filter: filter.value, limit: 100 });
	applications.value = res.applications;
}

async function approve(app: SignupApplication) {
	const { canceled } = await os.confirm({
		type: 'question',
		text: i18n.ts._mkgoSignupApplication._admin.approveConfirm,
	});
	if (canceled) return;
	await os.apiWithDialog('admin/signup-application/approve', { applicationId: app.id });
	await refresh();
}

async function reject(app: SignupApplication) {
	const { canceled } = await os.confirm({
		type: 'warning',
		text: i18n.ts._mkgoSignupApplication._admin.rejectConfirm,
	});
	if (canceled) return;
	await os.apiWithDialog('admin/signup-application/reject', { applicationId: app.id });
	await refresh();
}

watch(filter, refresh);

await refresh();

const headerActions = computed(() => [{
	icon: 'ti ti-refresh',
	text: i18n.ts._mkgoSignupApplication._admin.reload,
	handler: refresh,
}]);

const headerTabs = computed(() => []);

definePage(() => ({
	title: i18n.ts._mkgoSignupApplication._admin.title,
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
