<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div v-if="instance">
	<XSetup v-if="instance.requireSetup"/>
	<XEntranceClassic v-else-if="(instance.clientOptions.entrancePageStyle ?? 'classic') === 'classic'"/>
	<XEntranceSimple v-else/>
</div>
</template>

<script lang="ts" setup>
import { computed } from 'vue';
import { instanceName } from '@@/js/config.js';
import XSetup from './welcome.setup.vue';
import XEntranceClassic from './welcome.entrance.classic.vue';
import XEntranceSimple from './welcome.entrance.simple.vue';
import { definePage } from '@/page.js';
// Elythia: 読み込み済みのインスタンス情報ですぐ描く。本家は /api/meta を取り直してから描く (#3967)
import { useWelcomeInstance } from '@/utility/elythia-welcome-instance.js';

const instance = useWelcomeInstance();

const headerActions = computed(() => []);

const headerTabs = computed(() => []);

definePage(() => ({
	title: instanceName,
	icon: null,
}));
</script>
