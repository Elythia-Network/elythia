<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go (#3232): バブルゲームの対戦のリプレイ。両者の盤面を同じフレームで並べて
	再生する。記録が無い盤面 (切断した側など) と、別の版のゲームで遊ばれた盤面は
	再生しない (同じ記録でも版が違うと別の結末になる)。
-->
<template>
<div class="_spacer" style="--MI_SPACER-w: 800px;">
	<MkLoading v-if="record == null && loadFailed == null"/>
	<MkResult v-else-if="loadFailed === 'notFound'" type="notFound" :text="i18n.ts._mkgoBubbleGame._versus.notFound"/>
	<MkResult v-else-if="loadFailed != null" type="error"/>
	<div v-else-if="record != null" class="_gaps">
		<div class="_woodenFrame">
			<div class="_woodenFrameInner" style="text-align: center;">
				<b>{{ i18n.ts._mkgoBubbleGame._versus.replayTitle }}</b>
				<div>- {{ dropAndFusionModeLabel(record.gameMode) }} -</div>
				<div style="opacity: 0.8;"><MkTime :time="record.endedAt ?? record.startedAt" mode="detail"/></div>
			</div>
		</div>

		<div :class="$style.boards">
			<div v-for="side in sides" :key="side.index" :class="$style.board">
				<div class="_woodenFrame">
					<div class="_woodenFrameInner" :class="$style.player">
						<MkAvatar v-if="side.user" :user="side.user" :class="$style.avatar" :link="true"/>
						<span v-if="side.user" :class="$style.name"><MkUserName :user="side.user" :nowrap="true"/></span>
						<i v-if="record.winnerId != null && record.winnerId === side.userId" class="ti ti-trophy" style="color: var(--MI_THEME-accent);"></i>
						<span :class="$style.score"><MkNumber :value="side.playable ? (liveScores[side.index] ?? 0) : (side.result?.score ?? 0)"/></span>
					</div>
				</div>
				<XReplayBoard
					v-if="side.playable && side.logs != null && side.result != null"
					:ref="el => setBoard(side.index, el)"
					:key="`${side.index}:${playKey}`"
					:gameMode="record.gameMode"
					:seed="record.seed!"
					:logs="side.logs"
					:endFrame="side.result.frame"
					@score="v => liveScores[side.index] = v"
				/>
				<MkInfo v-else>{{ side.unplayableReason }}</MkInfo>
			</div>
		</div>

		<div class="_woodenFrame">
			<div class="_woodenFrameInner _gaps_s">
				<div :class="$style.progress">
					<div :class="$style.progressBar" :style="{ width: `${maxFrame > 0 ? (frame / maxFrame) * 100 : 0}%` }"></div>
				</div>
				<div style="font-size: 85%; opacity: 0.7;">{{ i18n.ts._mkgoBubbleGame._versus.replayFrameNote }}</div>
				<div class="_buttonsCenter">
					<MkButton :primary="rate === 1" @click="rate = 1"><i class="ti ti-player-play"></i> x1</MkButton>
					<MkButton :primary="rate === 4" @click="rate = 4"><i class="ti ti-player-track-next"></i> x4</MkButton>
					<MkButton :primary="rate === 16" @click="rate = 16"><i class="ti ti-player-track-next"></i> x16</MkButton>
					<MkButton @click="restart"><i class="ti ti-reload"></i> {{ i18n.ts._mkgoBubbleGame._versus.replayRestart }}</MkButton>
				</div>
			</div>
		</div>
	</div>
</div>
</template>

<script lang="ts" setup>
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, shallowRef } from 'vue';
import { DropAndFusionGame } from 'misskey-bubble-game';
import XReplayBoard from './drop-and-fusion.versus.replay-board.vue';
import { definePage } from '@/page.js';
import MkButton from '@/components/MkButton.vue';
import MkNumber from '@/components/MkNumber.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkResult from '@/components/global/MkResult.vue';
import { i18n } from '@/i18n.js';
import { dropAndFusionModeLabel } from '@/utility/drop-and-fusion-mode.js';
import { versusApi } from '@/utility/bubble-versus.js';
import type { VersusRecord } from '@/utility/bubble-versus.js';
import { replayPlayability } from '@/utility/bubble-versus-rules.js';

const props = defineProps<{
	matchId: string;
}>();

type Board = InstanceType<typeof XReplayBoard>;

const record = shallowRef<VersusRecord | null>(null);
const loadFailed = ref<'notFound' | 'error' | null>(null);
const frame = ref(0);
const rate = ref(1);
// 最初から再生し直すときに盤面を作り直すための印。
const playKey = ref(0);
const liveScores = reactive<Record<number, number>>({});
const boards: Record<number, Board | null> = {};
let raf: number | null = null;

const sides = computed(() => {
	const r = record.value;
	if (r == null) return [];
	return ([0, 1] as const).map(index => {
		const result = index === 0 ? r.user1Result : r.user2Result;
		const logs = (index === 0 ? r.user1Logs : r.user2Logs) ?? null;
		const playability = replayPlayability({ logs, result, engineVersion: DropAndFusionGame.VERSION });
		return {
			index,
			userId: index === 0 ? r.user1Id : r.user2Id,
			user: index === 0 ? r.user1 : r.user2,
			result,
			logs,
			playable: playability === 'ok',
			unplayableReason: playability === 'versionMismatch'
				? i18n.ts._mkgoBubbleGame._versus.replayVersionMismatch
				: i18n.ts._mkgoBubbleGame._versus.replayNoLogs,
		};
	});
});

// 再生できる盤面のうち、いちばん遅く終わったフレームまで進める。
const maxFrame = computed(() => Math.max(0, ...sides.value.filter(s => s.playable).map(s => s.result?.frame ?? 0)));

function setBoard(index: number, el: unknown) {
	boards[index] = (el as Board | null) ?? null;
}

function tick() {
	frame.value = Math.min(maxFrame.value, frame.value + rate.value);
	for (const board of Object.values(boards)) board?.advanceTo(frame.value);
	if (frame.value < maxFrame.value) {
		raf = window.requestAnimationFrame(tick);
	} else {
		raf = null;
	}
}

function stop() {
	if (raf != null) window.cancelAnimationFrame(raf);
	raf = null;
}

async function restart() {
	stop();
	frame.value = 0;
	for (const k of Object.keys(liveScores)) delete liveScores[Number(k)];
	playKey.value++;
	// 盤面を作り直してから進め始める。
	await nextTick();
	raf = window.requestAnimationFrame(tick);
}

onMounted(async () => {
	try {
		record.value = await versusApi<VersusRecord>('record', { matchId: props.matchId });
	} catch (err: any) {
		loadFailed.value = err?.code === 'NO_SUCH_MATCH' ? 'notFound' : 'error';
		return;
	}
	await nextTick();
	raf = window.requestAnimationFrame(tick);
});

onUnmounted(stop);

definePage(() => ({
	title: i18n.ts._mkgoBubbleGame._versus.replayTitle,
	icon: 'ti ti-player-play',
}));
</script>

<style lang="scss" module>
.boards {
	display: flex;
	gap: 12px;
}

.board {
	flex: 1;
	min-width: 0;
	display: flex;
	flex-direction: column;
	gap: 8px;
}

.player {
	display: flex;
	align-items: center;
	gap: 6px;
	min-width: 0;
}

.avatar {
	width: 28px;
	height: 28px;
	flex-shrink: 0;
}

.name {
	min-width: 0;
	overflow: hidden;
}

.score {
	margin-left: auto;
	font-weight: bold;
	font-variant-numeric: tabular-nums;
}

.progress {
	height: 10px;
	background: var(--MI_THEME-panel);
	border-radius: 999px;
	overflow: clip;
}

.progressBar {
	height: 100%;
	background: var(--MI_THEME-accent);
	will-change: width;
}
</style>
