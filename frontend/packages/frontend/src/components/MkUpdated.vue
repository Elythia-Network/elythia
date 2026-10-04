<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkModal ref="modal" preferType="dialog" :zPriority="'middle'" @click="modal?.close()" @closed="emit('closed')">
	<div :class="$style.root">
		<div :class="$style.title"><MkSparkle>{{ props.isMkGo ? i18n.ts.mkGoUpdated : i18n.ts.misskeyUpdated }}</MkSparkle></div>
		<div :class="$style.version">✨{{ props.isMkGo ? `mk-go ${shownVersion}` : shownVersion }}🚀</div>
		<div v-if="isBeta" :class="$style.beta">{{ i18n.ts.thankYouForTestingBeta }}</div>
		<MkButton full @click="whatIsNew">{{ i18n.ts.whatIsNew }}</MkButton>
		<MkButton :class="$style.gotIt" primary full @click="modal?.close()">{{ i18n.ts.gotIt }}</MkButton>
	</div>
</MkModal>
</template>

<script lang="ts" setup>
import { computed, onMounted, useTemplateRef } from 'vue';
import { version } from '@@/js/config.js';
import MkModal from '@/components/MkModal.vue';
import MkButton from '@/components/MkButton.vue';
import MkSparkle from '@/components/MkSparkle.vue';
import { i18n } from '@/i18n.js';
import { confetti } from '@/utility/confetti.js';
import { mkGoChangelogUrl } from '@/utility/check-client-update.js';

const modal = useTemplateRef('modal');

// **表示する版は呼び出し側から渡される (#2939)。** 既定は upstream と同じ
// build 定数で、純正 backend に繋いだときはこれまでどおりの見た目になる。
const props = withDefaults(defineProps<{
	isMkGo?: boolean;
	updatedVersion?: string;
}>(), {
	isMkGo: false,
	updatedVersion: undefined,
});

const emit = defineEmits<{
	(ev: 'closed'): void;
}>();

const shownVersion = computed(() => props.updatedVersion ?? version);

// beta / alpha / rc の判定は Misskey の版にしか無い概念。mk-go のリリースは
// 素の semver なので、そちらで出したときは常に false になる。
const isBeta = !props.isMkGo && (version.includes('-beta') || version.includes('-alpha') || version.includes('-rc'));

function whatIsNew() {
	modal.value?.close();
	if (props.isMkGo) {
		window.open(mkGoChangelogUrl(shownVersion.value), '_blank');
	} else if (isBeta) {
		window.open(`https://github.com/misskey-dev/misskey/releases/tag/${version}`, '_blank');
	} else {
		window.open(`https://misskey-hub.net/docs/releases/#_${version.replace(/\./g, '')}`, '_blank');
	}
}

onMounted(() => {
	confetti({
		duration: 1000 * 3,
	});
});
</script>

<style lang="scss" module>
.root {
	margin: auto;
	position: relative;
	padding: 32px;
	min-width: 320px;
	max-width: 480px;
	box-sizing: border-box;
	text-align: center;
	background: var(--MI_THEME-panel);
	border-radius: var(--MI-radius);
}

.title {
	font-weight: bold;
}

.version {
	margin: 1em 0;
}

.beta {
	margin: 1em 0;
}

.gotIt {
	margin: 8px 0 0 0;
}
</style>
