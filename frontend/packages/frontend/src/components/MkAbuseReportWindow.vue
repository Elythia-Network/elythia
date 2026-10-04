<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkWindow ref="uiWindow" :initialWidth="440" :initialHeight="620" :canResize="true" @closed="emit('closed')">
	<template #header>
		<i class="ti ti-exclamation-circle" style="margin-right: 0.5em;"></i>
		<I18n :src="i18n.ts.reportAbuseOf" tag="span">
			<template #name>
				<b><MkAcct :user="user"/></b>
			</template>
		</I18n>
	</template>
	<div class="_spacer" style="--MI_SPACER-min: 20px; --MI_SPACER-max: 28px;">
		<div class="_gaps_m" :class="$style.root">
			<MkSelect v-model="category" :items="categoryItems" :class="{ [$style.invalid]: showCategoryError }">
				<template #label>{{ i18n.ts._abuseReportForm.category }}</template>
			</MkSelect>
			<p v-if="showCategoryError" :class="$style.error">{{ i18n.ts._abuseReportForm.categoryRequired }}</p>

			<MkInput v-model="where">
				<template #label>{{ i18n.ts._abuseReportForm.url }}</template>
			</MkInput>

			<MkInput v-model="when">
				<template #label>{{ i18n.ts._abuseReportForm.when }}</template>
			</MkInput>

			<div data-testid="abuse-report-details">
				<MkTextarea v-model="details" :class="{ [$style.invalid]: showDetailsError }">
					<template #label>{{ i18n.ts._abuseReportForm.details }}</template>
					<template #caption>{{ i18n.ts._abuseReportForm.detailsCaption }}</template>
				</MkTextarea>
			</div>
			<p v-if="showDetailsError" :class="$style.error">{{ i18n.ts._abuseReportForm.detailsRequired }}</p>

			<MkTextarea v-model="evidence">
				<template #label>{{ i18n.ts._abuseReportForm.evidence }}</template>
				<template #caption>{{ i18n.ts._abuseReportForm.evidenceCaption }}</template>
			</MkTextarea>

			<p v-if="remaining >= 0" :class="[$style.counter, { [$style.counterWarn]: remaining < 128 }]">
				{{ i18n.tsx._abuseReportForm.remainingChars({ n: remaining }) }}
			</p>
			<p v-else :class="[$style.counter, $style.counterError]">
				{{ i18n.tsx._abuseReportForm.totalTooLong({ max: ABUSE_REPORT_COMMENT_MAX }) }}
			</p>

			<div>
				<MkButton primary full :disabled="!canSend" @click="send">{{ i18n.ts.send }}</MkButton>
			</div>
		</div>
	</div>
</MkWindow>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, useTemplateRef } from 'vue';
import * as Misskey from 'misskey-js';
import MkWindow from '@/components/MkWindow.vue';
import MkTextarea from '@/components/MkTextarea.vue';
import MkInput from '@/components/MkInput.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkButton from '@/components/MkButton.vue';
import * as os from '@/os.js';
import { i18n } from '@/i18n.js';
import {
	ABUSE_REPORT_CATEGORIES,
	ABUSE_REPORT_COMMENT_MAX,
	buildAbuseReportComment,
	buildContextFromProfile,
	getAbuseReportFormLocale,
	isAbuseReportFormValid,
	remainingAbuseReportRunes,
} from '@/utility/abuse-report.js';
import type { AbuseReportCategory, AbuseReportContext } from '@/utility/abuse-report.js';

const props = defineProps<{
	user: Misskey.entities.UserLite;
	context?: AbuseReportContext;
}>();

const emit = defineEmits<{
	(ev: 'closed'): void;
}>();

const uiWindow = useTemplateRef('uiWindow');
const reportContext = computed(() => props.context ?? buildContextFromProfile(props.user, window.location.origin));

const category = ref<AbuseReportCategory | ''>('');
const details = ref('');
const when = ref(reportContext.value.when ?? '');
const where = ref(reportContext.value.where ?? '');
const evidence = ref('');
const touched = ref(false);
const formLocale = ref<Awaited<ReturnType<typeof getAbuseReportFormLocale>> | null>(null);

const categoryItems = computed(() => ABUSE_REPORT_CATEGORIES.map(value => ({
	value,
	label: i18n.ts._abuseReportForm._category[value],
})));

const formValues = computed(() => ({
	category: category.value,
	details: details.value,
	when: when.value,
	where: where.value,
	evidence: evidence.value,
}));

const remaining = computed(() => {
	if (!formLocale.value) return ABUSE_REPORT_COMMENT_MAX;
	return remainingAbuseReportRunes(formLocale.value, formValues.value, reportContext.value);
});

const canSend = computed(() => formLocale.value != null && isAbuseReportFormValid(formLocale.value, formValues.value, reportContext.value));

const showCategoryError = computed(() => touched.value && category.value === '');
const showDetailsError = computed(() => touched.value && details.value.trim() === '');

onMounted(async () => {
	formLocale.value = await getAbuseReportFormLocale();
});

function send() {
	touched.value = true;
	if (!formLocale.value || !canSend.value) return;

	const comment = buildAbuseReportComment(formLocale.value, formValues.value, reportContext.value);
	os.apiWithDialog('users/report-abuse', {
		userId: props.user.id,
		comment,
	}, undefined).then(() => {
		os.alert({
			type: 'success',
			text: i18n.ts.abuseReported,
		});
		uiWindow.value?.close();
		emit('closed');
	});
}
</script>

<style lang="scss" module>
.root {
	--root-margin: 16px;
}

.invalid :global(.input),
.invalid :global(textarea) {
	border-color: var(--MI_THEME-error) !important;
}

.error {
	margin: 0;
	font-size: 0.85em;
	color: var(--MI_THEME-error);
}

.counter {
	margin: 0;
	font-size: 0.85em;
	color: var(--MI_THEME-fgTransparentWeak);
	text-align: right;
}

.counterWarn {
	color: var(--MI_THEME-warn);
}

.counterError {
	color: var(--MI_THEME-error);
}
</style>
