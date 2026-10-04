<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go (#3232): バブルゲームの対戦の履歴。自分の履歴では対局ごとに公開の意思を
	切り替えられる。他の人の履歴には、その人と相手の 2 人とも公開にした対局だけが
	並ぶ (選別はサーバーがする)。
-->
<template>
<div class="_spacer" style="--MI_SPACER-w: 600px;">
	<div class="_gaps">
		<MkInfo>{{ i18n.ts._mkgoBubbleGame._versus.historyDescription }}</MkInfo>

		<MkLoading v-if="records == null && loadFailed == null"/>
		<MkResult v-else-if="loadFailed === 'notFound'" type="notFound"/>
		<MkResult v-else-if="loadFailed != null" type="error"/>
		<MkResult v-else-if="records != null && records.length === 0" type="empty" :text="i18n.ts._mkgoBubbleGame._versus.noHistory"/>

		<template v-else-if="records != null">
			<div v-for="r in records" :key="r.id" class="_panel" :class="$style.item">
				<div :class="$style.head">
					<span :class="$style.outcome">{{ versusOutcomeLabel(outcomeOf(r)) }}</span>
					<span style="opacity: 0.7;">{{ dropAndFusionModeLabel(r.gameMode) }}</span>
					<MkTime :time="r.endedAt ?? r.startedAt" :class="$style.time"/>
				</div>
				<div :class="$style.players">
					<div v-for="i in [0, 1]" :key="i" :class="$style.player">
						<MkAvatar v-if="userOf(r, i)" :user="userOf(r, i)!" :class="$style.avatar" :link="true"/>
						<span v-if="userOf(r, i)" :class="$style.name"><MkUserName :user="userOf(r, i)!" :nowrap="true"/></span>
						<i v-if="r.winnerId != null && r.winnerId === idOf(r, i)" class="ti ti-trophy" style="color: var(--MI_THEME-accent);"></i>
						<span :class="$style.score"><MkNumber v-if="resultOf(r, i)" :value="resultOf(r, i)!.score"/><template v-else>-</template></span>
					</div>
				</div>
				<div style="opacity: 0.8; font-size: 90%;">{{ versusReasonLabel(r.reason) }}</div>
				<div :class="$style.foot">
					<MkSwitch v-if="isMe" :modelValue="myPublic(r)" :class="$style.public" @update:modelValue="v => setPublic(r, v)">
						<template #label>{{ i18n.ts._mkgoBubbleGame._versus.publicSwitch }}</template>
						<template #caption>{{ r.isPublic ? i18n.ts._mkgoBubbleGame._versus.publicBoth : myPublic(r) ? i18n.ts._mkgoBubbleGame._versus.publicWaiting : '' }}</template>
					</MkSwitch>
					<MkButton rounded small @click="openReplay(r)"><i class="ti ti-player-play"></i> {{ i18n.ts._mkgoBubbleGame._versus.showReplay }}</MkButton>
				</div>
			</div>
			<MkButton v-if="hasMore" :class="$style.more" rounded :wait="loadingMore" @click="loadMore">{{ i18n.ts.loadMore }}</MkButton>
		</template>
	</div>
</div>
</template>

<script lang="ts" setup>
import { computed, onMounted, ref, shallowRef, triggerRef } from 'vue';
import * as Misskey from 'misskey-js';
import { definePage } from '@/page.js';
import MkButton from '@/components/MkButton.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkNumber from '@/components/MkNumber.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import MkResult from '@/components/global/MkResult.vue';
import { i18n } from '@/i18n.js';
import { $i } from '@/i.js';
import * as os from '@/os.js';
import { useRouter } from '@/router.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { dropAndFusionModeLabel } from '@/utility/drop-and-fusion-mode.js';
import { versusApi } from '@/utility/bubble-versus.js';
import type { VersusRecord, VersusRecordResult } from '@/utility/bubble-versus.js';
import { versusOutcomeLabel, versusReasonLabel } from '@/utility/bubble-versus-labels.js';
import { outcomeFor } from '@/utility/bubble-versus-rules.js';

const props = defineProps<{
	/** Whose history to show. Omitted: the signed-in user. */
	userId?: string;
}>();

const PAGE_SIZE = 20;

const router = useRouter();
const targetId = computed(() => props.userId ?? $i?.id ?? null);
const isMe = computed(() => targetId.value != null && targetId.value === $i?.id);
const targetUser = shallowRef<Misskey.entities.UserDetailed | null>(null);
const records = shallowRef<VersusRecord[] | null>(null);
const loadFailed = ref<'notFound' | 'error' | null>(null);
const hasMore = ref(false);
const loadingMore = ref(false);

function idOf(r: VersusRecord, i: number): string {
	return i === 0 ? r.user1Id : r.user2Id;
}

function userOf(r: VersusRecord, i: number): Misskey.entities.UserLite | null {
	return i === 0 ? r.user1 : r.user2;
}

function resultOf(r: VersusRecord, i: number): VersusRecordResult | null {
	return i === 0 ? r.user1Result : r.user2Result;
}

// 履歴の持ち主から見た勝ち負け (見ている人ではなく)。
function outcomeOf(r: VersusRecord) {
	return outcomeFor({ status: 'ended', winnerId: r.winnerId }, targetId.value);
}

function myPublic(r: VersusRecord): boolean {
	return $i?.id === r.user1Id ? r.user1Public : r.user2Public;
}

async function fetchPage(untilId?: string): Promise<VersusRecord[]> {
	return versusApi<VersusRecord[]>('history', {
		...(props.userId != null ? { userId: props.userId } : {}),
		limit: PAGE_SIZE,
		...(untilId != null ? { untilId } : {}),
	});
}

async function load() {
	try {
		const page = await fetchPage();
		records.value = page;
		// サーバーは見せない対局を飛ばした分を引き直して limit 件まで集めるので、
		// それより短いページは最後のページ。
		hasMore.value = page.length >= PAGE_SIZE;
	} catch (err: any) {
		loadFailed.value = err?.code === 'NO_SUCH_USER' ? 'notFound' : 'error';
	}
}

async function loadMore() {
	if (records.value == null || records.value.length === 0) return;
	loadingMore.value = true;
	try {
		const page = await fetchPage(records.value[records.value.length - 1].id);
		records.value = [...records.value, ...page];
		hasMore.value = page.length >= PAGE_SIZE;
	} catch {
		os.toast(i18n.ts._mkgoBubbleGame._versus.operationFailed);
	} finally {
		loadingMore.value = false;
	}
}

async function setPublic(r: VersusRecord, isPublic: boolean) {
	try {
		await versusApi('set-public', { matchId: r.id, isPublic });
	} catch {
		os.toast(i18n.ts._mkgoBubbleGame._versus.operationFailed);
		return;
	}
	if ($i?.id === r.user1Id) r.user1Public = isPublic;
	else r.user2Public = isPublic;
	r.isPublic = r.user1Public && r.user2Public;
	triggerRef(records);
}

function openReplay(r: VersusRecord) {
	router.push('/bubble-game/versus/replay/:matchId', { params: { matchId: r.id } });
}

onMounted(async () => {
	if (props.userId != null && !isMe.value) {
		try {
			targetUser.value = await misskeyApi('users/show', { userId: props.userId });
		} catch {
			loadFailed.value = 'notFound';
			return;
		}
	}
	await load();
});

definePage(() => ({
	title: targetUser.value != null
		? i18n.tsx._mkgoBubbleGame._versus.historyOf({ name: targetUser.value.name ?? targetUser.value.username })
		: i18n.ts._mkgoBubbleGame._versus.history,
	icon: 'ti ti-history',
}));
</script>

<style lang="scss" module>
.item {
	display: flex;
	flex-direction: column;
	gap: 8px;
	padding: 12px 16px;
}

.head {
	display: flex;
	align-items: baseline;
	gap: 8px;
}

.outcome {
	font-weight: bold;
}

.time {
	margin-left: auto;
	opacity: 0.7;
	font-size: 90%;
}

.players {
	display: flex;
	flex-direction: column;
	gap: 4px;
}

.player {
	display: flex;
	align-items: center;
	gap: 6px;
	min-width: 0;
}

.avatar {
	width: 24px;
	height: 24px;
	flex-shrink: 0;
}

.name {
	min-width: 0;
	overflow: hidden;
}

.score {
	margin-left: auto;
	font-variant-numeric: tabular-nums;
}

.foot {
	display: flex;
	align-items: center;
	gap: 8px;
}

.public {
	flex: 1;
	min-width: 0;
}

.more {
	margin: 0 auto;
}
</style>
