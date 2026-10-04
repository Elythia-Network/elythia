<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader v-model:tab="tab" :actions="headerActions" :tabs="headerTabs">
	<div class="_spacer" style="--MI_SPACER-w: 800px; --MI_SPACER-min: 16px; --MI_SPACER-max: 32px;">
		<!-- DB の健全性 (mk-go #3095)。 -->
		<XDatabaseHealth v-if="tab === 'health'"/>
		<MkSuspense v-else v-slot="{ result: database }" :p="databasePromiseFactory">
			<MkKeyValue v-for="table in database" :key="table[0]" oneline style="margin: 1em 0;">
				<template #key>{{ table[0] }}</template>
				<template #value>{{ bytes(table[1].size) }} ({{ number(table[1].count) }} recs)</template>
			</MkKeyValue>
		</MkSuspense>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { computed, ref } from 'vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import XDatabaseHealth from '@/pages/admin/database.health.vue';
import { misskeyApi } from '@/utility/misskey-api.js';
import bytes from '@/filters/bytes.js';
import number from '@/filters/number.js';
import { i18n } from '@/i18n.js';
import { definePage } from '@/page.js';

const databasePromiseFactory = () => misskeyApi('admin/get-table-stats').then(res => Object.entries(res).sort((a, b) => b[1].size - a[1].size));

const headerActions = computed(() => []);

const tab = ref<'tables' | 'health'>('tables');

const headerTabs = computed(() => [{
	key: 'tables',
	title: i18n.ts._databaseHealth.tabTables,
}, {
	key: 'health',
	title: i18n.ts._databaseHealth.tabHealth,
}]);

definePage(() => ({
	title: i18n.ts.database,
	icon: 'ti ti-database',
}));
</script>
