<!--
SPDX-FileCopyrightText: syuilo and other misskey contributors
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader v-model:tab="headerTab" :tabs="headerTabs">
	<XGridLocalComponent v-if="headerTab === 'local'" :class="$style.local"/>
	<XGridRemoteComponent v-else-if="headerTab === 'remote'"/>
	<XRegisterComponent v-else-if="headerTab === 'register'"/>
	<XApplicationsComponent v-else-if="headerTab === 'applications'"/>
</PageWithHeader>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue';
import { i18n } from '@/i18n.js';
import { definePage } from '@/page.js';
import XGridLocalComponent from '@/pages/admin/custom-emojis-manager.local.list.vue';
import XGridRemoteComponent from '@/pages/admin/custom-emojis-manager.remote.vue';
import XRegisterComponent from '@/pages/admin/custom-emojis-manager.register.vue';
import XApplicationsComponent from '@/pages/admin/custom-emojis-manager.applications.vue';

type PageMode = 'local' | 'remote' | 'register' | 'applications';

const headerTab = ref<PageMode>('local');

const headerTabs = computed(() => [{
	key: 'local',
	title: i18n.ts.local,
}, {
	key: 'remote',
	title: i18n.ts.remote,
}, {
	key: 'register',
	title: i18n.ts._customEmojisManager._local.tabTitleRegister,
}, {
	// 登録申請の審査 (#2934)。**既存の絵文字と見比べる場面が多いので同じ画面に
	// 置く。** 別ページにすると、重複の確認のたびに行き来することになる。
	key: 'applications',
	title: i18n.ts._emojiApplication.tabTitle,
}]);

definePage(computed(() => ({
	title: i18n.ts.customEmojis,
	icon: 'ti ti-icons',
	needWideArea: true,
})));
</script>

<style lang="css" module>
.local {
	height: calc(100dvh - var(--MI-stickyTop) - var(--MI-stickyBottom));
	overflow: clip;
}
</style>
