<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader v-model:tab="tab" :actions="headerActions" :tabs="headerTabs">
	<div class="_spacer" style="--MI_SPACER-w: 900px;">
		<!--
			サーバープラグインの描画先 (mk-go #2543)。連合の状態を見に来る
			のはこのページなので、概要は検索欄より前に置く。**タブの外に置く** —
			中に入れると instances タブ限定になり、タブを行き来するたびに
			unmount されてプラグイン側の状態も飛ぶ (#2944)。
		-->
		<div class="_gaps">
			<!--
				**_gaps で包む。** MkPluginSlot の root は display: contents で自分の箱を
				持たないので、flex/gap のある親の直下でないとプラグイン出力どうし・
				下のコンテンツとの余白が 0 になる。_spacer は padding しか持たない。
			-->
			<MkPluginSlot name="admin:federation"/>

			<div v-if="tab === 'instances'" class="_gaps">
				<div>
					<MkInput v-model="host" :debounce="true" class="">
						<template #prefix><i class="ti ti-search"></i></template>
						<template #label>{{ i18n.ts.host }}</template>
					</MkInput>
					<FormSplit style="margin-top: var(--MI-margin);">
						<MkSelect v-model="state" :items="stateDef">
							<template #label>{{ i18n.ts.state }}</template>
						</MkSelect>
						<MkSelect v-model="sort" :items="sortDef">
							<template #label>{{ i18n.ts.sort }}</template>
						</MkSelect>
					</FormSplit>
				</div>

				<MkPagination v-slot="{items}" :key="host + state" :paginator="paginator">
					<div :class="$style.instances">
						<MkA v-for="instance in items" :key="instance.id" v-tooltip.mfm="`Status: ${getStatus(instance)}`" :class="$style.instance" :to="`/instance-info/${instance.host}`">
							<MkInstanceCardMini :instance="instance"/>
						</MkA>
					</div>
				</MkPagination>
			</div>

			<!--
				配送の健全性 (mk-go #2944)。**名簿 (instances) と観測 (deliver /
				inbox) を同じページに置く。** 出どころが DB と Redis で違い、
				「一覧に無い = 正常」ではない点を取り違えやすいので、行き来できる
				ところに並べて注意書きを添える。host は共有して、調べている相手を
				タブ間で持ち越す。
			-->
			<!-- 消えたサーバーとのフォロー関係の片付け (mk-go #3067)。 -->
			<XFederationGone v-else-if="tab === 'gone'"/>
			<!-- 連合のルール (mk-go #3090)。 -->
			<XFederationRules v-else-if="tab === 'rules'"/>
			<XFederationHealth v-else :key="tab" v-model:host="host" :direction="tab"/>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import * as Misskey from 'misskey-js';
import { computed, markRaw, ref } from 'vue';
import MkInput from '@/components/MkInput.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkPagination from '@/components/MkPagination.vue';
import MkInstanceCardMini from '@/components/MkInstanceCardMini.vue';
import MkPluginSlot from '@/components/MkPluginSlot.vue';
import XFederationHealth from '@/pages/admin/federation.health.vue';
import XFederationGone from '@/pages/admin/federation.gone.vue';
import XFederationRules from '@/pages/admin/federation.rules.vue';
import FormSplit from '@/components/form/split.vue';
import { i18n } from '@/i18n.js';
import { definePage } from '@/page.js';
import { useMkSelect } from '@/composables/use-mkselect.js';
import { Paginator } from '@/utility/paginator.js';

const tab = ref<'instances' | 'deliver' | 'inbox' | 'gone' | 'rules'>('instances');
const host = ref('');
const {
	model: state,
	def: stateDef,
} = useMkSelect({
	items: [
		{ label: i18n.ts.all, value: 'all' },
		{ label: i18n.ts.federating, value: 'federating' },
		{ label: i18n.ts.subscribing, value: 'subscribing' },
		{ label: i18n.ts.publishing, value: 'publishing' },
		{ label: i18n.ts.suspended, value: 'suspended' },
		{ label: i18n.ts.blocked, value: 'blocked' },
		{ label: i18n.ts.silence, value: 'silenced' },
		{ label: i18n.ts.notResponding, value: 'notResponding' },
	],
	initialValue: 'federating',
});
const {
	model: sort,
	def: sortDef,
} = useMkSelect({
	items: [
		{ label: `${i18n.ts.pubSub} (${i18n.ts.descendingOrder})`, value: '+pubSub' },
		{ label: `${i18n.ts.pubSub} (${i18n.ts.ascendingOrder})`, value: '-pubSub' },
		{ label: `${i18n.ts.notes} (${i18n.ts.descendingOrder})`, value: '+notes' },
		{ label: `${i18n.ts.notes} (${i18n.ts.ascendingOrder})`, value: '-notes' },
		{ label: `${i18n.ts.users} (${i18n.ts.descendingOrder})`, value: '+users' },
		{ label: `${i18n.ts.users} (${i18n.ts.ascendingOrder})`, value: '-users' },
		{ label: `${i18n.ts.following} (${i18n.ts.descendingOrder})`, value: '+following' },
		{ label: `${i18n.ts.following} (${i18n.ts.ascendingOrder})`, value: '-following' },
		{ label: `${i18n.ts.followers} (${i18n.ts.descendingOrder})`, value: '+followers' },
		{ label: `${i18n.ts.followers} (${i18n.ts.ascendingOrder})`, value: '-followers' },
		{ label: `${i18n.ts.registeredAt} (${i18n.ts.descendingOrder})`, value: '+firstRetrievedAt' },
		{ label: `${i18n.ts.registeredAt} (${i18n.ts.ascendingOrder})`, value: '-firstRetrievedAt' },
	],
	initialValue: '+pubSub',
});
const paginator = markRaw(new Paginator('federation/instances', {
	limit: 10,
	offsetMode: true,
	computedParams: computed(() => ({
		sort: sort.value,
		host: host.value !== '' ? host.value : null,
		...(
			state.value === 'federating' ? { federating: true, suspended: false, blocked: false } :
			state.value === 'subscribing' ? { subscribing: true, suspended: false, blocked: false } :
			state.value === 'publishing' ? { publishing: true, suspended: false, blocked: false } :
			state.value === 'suspended' ? { suspended: true } :
			state.value === 'blocked' ? { blocked: true } :
			state.value === 'silenced' ? { silenced: true } :
			state.value === 'notResponding' ? { notResponding: true } :
			{}),
	})),
}));

function getStatus(instance: Misskey.entities.FederationInstance) {
	switch (instance.suspensionState) {
		case 'manuallySuspended':
			return 'Manually Suspended';
		case 'goneSuspended':
			return 'Automatically Suspended (Gone)';
		case 'autoSuspendedForNotResponding':
			return 'Automatically Suspended (Not Responding)';
		case 'none':
			break;
	}
	if (instance.isBlocked) return 'Blocked';
	if (instance.isSilenced) return 'Silenced';
	if (instance.isNotResponding) return 'Error';
	return 'Alive';
}

const headerActions = computed(() => []);

const headerTabs = computed(() => [{
	key: 'instances',
	title: i18n.ts.instances,
}, {
	// 連合ジョブの画面と同じ名前にする (federation-job-queue.vue も i18n を
	// 通さず Deliver / Inbox のまま)。運営者が同じものを 2 つの言葉で
	// 覚えることにならないようにする。
	key: 'deliver',
	title: 'Deliver',
}, {
	key: 'inbox',
	title: 'Inbox',
}, {
	key: 'gone',
	title: i18n.ts._goneInstances.tab,
}, {
	key: 'rules',
	title: i18n.ts._federationRules.tab,
}]);

definePage(() => ({
	title: i18n.ts.federation,
	icon: 'ti ti-whirl',
}));
</script>

<style lang="scss" module>
.instances {
	display: grid;
	grid-template-columns: repeat(auto-fill, minmax(270px, 1fr));
	grid-gap: 12px;
}

.instance:hover {
	text-decoration: none;
}
</style>
