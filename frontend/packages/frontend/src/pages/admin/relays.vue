<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader :actions="headerActions" :tabs="headerTabs">
	<div class="_spacer" style="--MI_SPACER-w: 800px;">
		<SearchMarker path="/admin/relays" :label="i18n.ts.relays" :keywords="['relays']" icon="ti ti-planet">
			<div class="_gaps">
				<!-- mk-go 独自: リレー経由投稿を DB に保存せず Redis で揮発させる (#2332)。
				     設定対象がリレー経由の投稿なので、購読の管理と同じ画面に置く。 -->
				<MkFolder>
					<template #icon><i class="ti ti-flame"></i></template>
					<template #label><SearchLabel>{{ i18n.ts.ephemeralRelayNotes }}</SearchLabel></template>

					<div class="_gaps_m">
						<MkInfo warn>{{ i18n.ts.ephemeralRelayNotesWarning }}</MkInfo>

						<SearchMarker>
							<MkSwitch v-model="enableEphemeralRelayNotes">
								<SearchLabel>{{ i18n.ts.enableEphemeralRelayNotes }}</SearchLabel>
								<template #caption><SearchText>{{ i18n.ts.enableEphemeralRelayNotesDescription }}</SearchText></template>
							</MkSwitch>
						</SearchMarker>

						<template v-if="enableEphemeralRelayNotes">
							<SearchMarker>
								<MkInput v-model="ephemeralRelayNoteTtlMinutes" type="number" :min="1">
									<template #label><SearchLabel>{{ i18n.ts.ephemeralRelayNoteTtl }}</SearchLabel></template>
									<template #suffix>{{ i18n.ts._time.minute }}</template>
									<template #caption><SearchText>{{ i18n.ts.ephemeralRelayNoteTtlDescription }}</SearchText></template>
								</MkInput>
							</SearchMarker>
						</template>

						<MkButton primary @click="saveEphemeralSettings"><i class="ti ti-check"></i> {{ i18n.ts.save }}</MkButton>
					</div>
				</MkFolder>

				<!-- mk-go 独自: リレー由来の孤児リモートユーザーを定期削除する (shiroha-a/mk#2340)。
				     転送活動の署名検証は著者の公開鍵を DB に載せる必要があり、その経路だけは
				     揮発化できないため後追いで回収する。 -->
				<MkFolder>
					<template #icon><i class="ti ti-user-off"></i></template>
					<template #label><SearchLabel>{{ i18n.ts.relayOrphanUserCleanup }}</SearchLabel></template>

					<div class="_gaps_m">
						<MkInfo>{{ i18n.ts.relayOrphanUserCleanupInfo }}</MkInfo>

						<SearchMarker>
							<MkSwitch v-model="enableRelayOrphanUserCleanup">
								<SearchLabel>{{ i18n.ts.enableRelayOrphanUserCleanup }}</SearchLabel>
								<template #caption><SearchText>{{ i18n.ts.enableRelayOrphanUserCleanupDescription }}</SearchText></template>
							</MkSwitch>
						</SearchMarker>

						<template v-if="enableRelayOrphanUserCleanup">
							<SearchMarker>
								<MkInput v-model="relayOrphanUserGraceDays" type="number" :min="7">
									<template #label><SearchLabel>{{ i18n.ts.relayOrphanUserGraceDays }}</SearchLabel></template>
									<template #suffix>{{ i18n.ts._time.day }}</template>
									<template #caption><SearchText>{{ i18n.ts.relayOrphanUserGraceDaysDescription }}</SearchText></template>
								</MkInput>
							</SearchMarker>
						</template>

						<MkButton primary @click="saveOrphanCleanupSettings"><i class="ti ti-check"></i> {{ i18n.ts.save }}</MkButton>
					</div>
				</MkFolder>

				<div v-for="relay in relays" :key="relay.inbox" class="relaycxt _panel" style="padding: 16px;">
					<div>{{ relay.inbox }}</div>
					<div style="margin: 8px 0;">
						<i v-if="relay.status === 'accepted'" class="ti ti-check" :class="$style.icon" style="color: var(--MI_THEME-success);"></i>
						<i v-else-if="relay.status === 'rejected'" class="ti ti-ban" :class="$style.icon" style="color: var(--MI_THEME-error);"></i>
						<i v-else class="ti ti-clock" :class="$style.icon"></i>
						<span>{{ i18n.ts._relayStatus[relay.status] }}</span>
					</div>
					<MkButton class="button" inline danger @click="remove(relay.inbox)"><i class="ti ti-trash"></i> {{ i18n.ts.remove }}</MkButton>
				</div>
			</div>
		</SearchMarker>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { ref, computed } from 'vue';
import * as Misskey from 'misskey-js';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkInput from '@/components/MkInput.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import { definePage } from '@/page.js';

const relays = ref<Misskey.entities.AdminRelaysListResponse>([]);

// mk-go 独自の meta 列 (#2332)。misskey-js の autogen 型は upstream backend の
// OpenAPI 由来で mk-go では再生成できないため、any 経由で読む。
const enableEphemeralRelayNotes = ref(false);
const ephemeralRelayNoteTtlMinutes = ref(60);
const enableRelayOrphanUserCleanup = ref(false);
const relayOrphanUserGraceDays = ref(30);

async function loadEphemeralSettings() {
	const meta = await misskeyApi('admin/meta') as any;
	enableEphemeralRelayNotes.value = meta.enableEphemeralRelayNotes ?? false;
	ephemeralRelayNoteTtlMinutes.value = meta.ephemeralRelayNoteTtlMinutes ?? 60;
	enableRelayOrphanUserCleanup.value = meta.enableRelayOrphanUserCleanup ?? false;
	relayOrphanUserGraceDays.value = meta.relayOrphanUserGraceDays ?? 30;
}

function saveEphemeralSettings() {
	os.apiWithDialog('admin/update-meta', {
		enableEphemeralRelayNotes: enableEphemeralRelayNotes.value,
		ephemeralRelayNoteTtlMinutes: ephemeralRelayNoteTtlMinutes.value,
	} as any);
}

function saveOrphanCleanupSettings() {
	os.apiWithDialog('admin/update-meta', {
		enableRelayOrphanUserCleanup: enableRelayOrphanUserCleanup.value,
		relayOrphanUserGraceDays: relayOrphanUserGraceDays.value,
	} as any);
}

async function addRelay() {
	const { canceled, result: inbox } = await os.inputText({
		title: i18n.ts.addRelay,
		type: 'url',
		placeholder: i18n.ts.inboxUrl,
	});
	if (canceled || inbox == null) return;
	misskeyApi('admin/relays/add', {
		inbox,
	}).then(() => {
		refresh();
	}).catch((err: any) => {
		os.alert({
			type: 'error',
			text: err.message || err,
		});
	});
}

function remove(inbox: string) {
	misskeyApi('admin/relays/remove', {
		inbox,
	}).then(() => {
		refresh();
	}).catch((err: any) => {
		os.alert({
			type: 'error',
			text: err.message || err,
		});
	});
}

function refresh() {
	misskeyApi('admin/relays/list').then(relayList => {
		relays.value = relayList;
	});
}

refresh();
loadEphemeralSettings();

const headerActions = computed(() => [{
	asFullButton: true,
	icon: 'ti ti-plus',
	text: i18n.ts.addRelay,
	handler: addRelay,
}]);

const headerTabs = computed(() => []);

definePage(() => ({
	title: i18n.ts.relays,
	icon: 'ti ti-planet',
}));
</script>

<style lang="scss" module>
.icon {
	width: 1em;
	margin-right: 0.75em;
}
</style>
