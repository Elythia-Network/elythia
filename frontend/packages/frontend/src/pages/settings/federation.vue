<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div class="_gaps_m">
	<MkLoading v-if="loading && !state" />
	<MkError v-else-if="loadError && !state" @retry="load" />
	<div v-else-if="state" class="_gaps_m">
		<MkKeyValue>
			<template #key>{{ i18n.ts._localOnlyAccount.settingsTitle }}</template>
			<template #value>{{ statusLabel }}</template>
		</MkKeyValue>
		<MkInfo>{{ i18n.ts._localOnlyAccount.settingsDescription }}</MkInfo>
		<MkInfo v-if="state?.isSystemAccount" warn>{{ i18n.ts._localOnlyAccount.systemAccount }}</MkInfo>
		<MkInfo v-else-if="state?.firstExternalReferenceAt" warn>
			{{ i18n.tsx._localOnlyAccount.firstReference({ reason: firstExternalReferenceReasonLabel, host: state.firstExternalReferenceHost ?? '', time: new Date(state.firstExternalReferenceAt).toLocaleString() }) }}
		</MkInfo>
		<MkInfo v-else-if="state?.federationStatus === 'on'" warn>{{ i18n.ts._localOnlyAccount.enableWarning }}</MkInfo>
		<div class="_buttons">
			<MkButton v-if="state?.federationStatus !== 'on'" primary :disabled="state?.isSystemAccount || loading" @click="setStatus('on')">{{ i18n.ts._localOnlyAccount.turnOn }}</MkButton>
			<MkButton v-if="state?.federationStatus !== 'off'" danger :disabled="state?.isSystemAccount || loading || (state?.federationStatus === 'on' && !canDisableFederation(state))" @click="setStatus('off')">{{ i18n.ts._localOnlyAccount.turnOff }}</MkButton>
		</div>
	</div>
</div>
</template>

<script lang="ts" setup>
import { computed, onMounted, ref } from 'vue';
import type { AccountFederationState } from 'elythia-js';
import MkButton from '@/components/MkButton.vue';
import MkError from '@/components/global/MkError.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkLoading from '@/components/global/MkLoading.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import { i18n } from '@/i18n.js';
import { ensureSignin } from '@/i.js';
import * as os from '@/os.js';
import { canDisableFederation, confirmFederationDisable, federationReferenceReasonLabelKey, updateFederationWithRefresh } from '@/utility/account-federation.js';
import type { FederationReferenceReasonLabelKey } from '@/utility/account-federation.js';
import { misskeyApi } from '@/utility/misskey-api.js';

const $i = ensureSignin();
const state = ref<AccountFederationState | null>(null);
const loading = ref(true);
const loadError = ref(false);

const statusLabel = computed(() => state.value?.federationStatus === 'on'
	? i18n.ts._localOnlyAccount.statusOn
	: state.value?.federationStatus === 'off'
		? i18n.ts._localOnlyAccount.statusOff
		: i18n.ts._localOnlyAccount.statusPending);
const firstExternalReferenceReasonLabels = {
	reasonMigration: i18n.ts._localOnlyAccount.reasonMigration,
	reasonWebFinger: i18n.ts._localOnlyAccount.reasonWebFinger,
	reasonActor: i18n.ts._localOnlyAccount.reasonActor,
	reasonCollection: i18n.ts._localOnlyAccount.reasonCollection,
	reasonNote: i18n.ts._localOnlyAccount.reasonNote,
	reasonInbox: i18n.ts._localOnlyAccount.reasonInbox,
	reasonDelivery: i18n.ts._localOnlyAccount.reasonDelivery,
	reasonUnknown: i18n.ts._localOnlyAccount.reasonUnknown,
} satisfies Record<FederationReferenceReasonLabelKey, string>;
const firstExternalReferenceReasonLabel = computed(() => firstExternalReferenceReasonLabels[federationReferenceReasonLabelKey(state.value?.firstExternalReferenceReason)]);

async function load(): Promise<void> {
	loading.value = true;
	loadError.value = false;
	try {
		state.value = await misskeyApi('i/federation', {});
		$i.federationStatus = state.value.federationStatus;
	} catch {
		state.value = null;
		loadError.value = true;
	} finally {
		loading.value = false;
	}
}

async function setStatus(federationStatus: 'on' | 'off'): Promise<void> {
	if (!state.value || state.value.isSystemAccount) return;
	if (federationStatus === 'off' && state.value.federationStatus === 'on' && !canDisableFederation(state.value)) return;
	if (federationStatus === 'on') {
		const { canceled } = await os.confirm({
			type: 'warning',
			text: i18n.ts._localOnlyAccount.confirmTurnOn,
		});
		if (canceled) return;
	}
	const canProceed = await confirmFederationDisable(state.value.federationStatus, federationStatus, async () => {
		const { canceled } = await os.confirm({
			type: 'warning',
			text: i18n.ts._localOnlyAccount.confirmTurnOff,
		});
		return !canceled;
	});
	if (!canProceed) return;
	loading.value = true;
	try {
		state.value = await updateFederationWithRefresh(
			() => os.apiWithDialog('i/update-federation', { federationStatus }),
			load,
		);
		$i.federationStatus = state.value.federationStatus;
	} finally {
		loading.value = false;
	}
}

onMounted(() => {
	void load();
});
</script>
