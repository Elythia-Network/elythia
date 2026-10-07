<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<Transition
	:enterActiveClass="$style.transition_zoom_enterActive"
	:leaveActiveClass="$style.transition_zoom_leaveActive"
	:enterFromClass="$style.transition_zoom_enterFrom"
	:leaveToClass="$style.transition_zoom_leaveTo"
	:moveClass="$style.transition_zoom_move"
	mode="out-in"
>
	<div v-if="!gameStarted" class="_spacer" style="--MI_SPACER-w: 800px;">
		<div :class="$style.root">
			<div class="_gaps">
				<div class="_woodenFrame" style="text-align: center;">
					<div class="_woodenFrameInner">
						<img src="/client-assets/drop-and-fusion/logo.png" style="display: block; max-width: 100%; max-height: 200px; margin: auto;"/>
					</div>
				</div>
				<div class="_woodenFrame" style="text-align: center;">
					<div class="_woodenFrameInner">
						<div class="_gaps" style="padding: 16px;">
							<MkSelect v-model="baseMode" :items="baseModeDef">
								<template #label>{{ i18n.ts._mkgoBubbleGame.mode }}</template>
							</MkSelect>
							<!--
								mk-go (#3216): 物理は形と別に選ぶ。SPACE は重力がほぼ無いこと自体が
								物理の違いなので選ばせない。
							-->
							<MkSelect v-if="baseMode !== 'space'" v-model="physics" :items="physicsDef">
								<template #label>{{ i18n.ts._mkgoBubbleGame.physics }}</template>
								<template #caption>{{ physicsCaption }}</template>
							</MkSelect>
							<MkButton primary gradate large rounded inline @click="start">{{ i18n.ts.start }}</MkButton>
						</div>
					</div>
					<div class="_woodenFrameInner">
						<div class="_gaps" style="padding: 16px;">
							<div style="font-size: 90%;"><i class="ti ti-music"></i> {{ i18n.ts.soundWillBePlayed }}</div>
							<MkSwitch v-model="mute">
								<template #label>{{ i18n.ts.mute }}</template>
							</MkSwitch>
						</div>
					</div>
				</div>
				<!-- mk-go (#3231): 1:1 の対戦。モードと物理は上で選んだものを使う。 -->
				<div class="_woodenFrame">
					<div class="_woodenFrameInner">
						<div class="_gaps_s" style="padding: 16px;">
							<div><b>{{ i18n.ts._mkgoBubbleGame._versus.section }}</b></div>
							<div style="font-size: 90%;">{{ i18n.ts._mkgoBubbleGame._versus.description }}</div>
							<div style="font-size: 90%; opacity: 0.8;">{{ i18n.tsx._mkgoBubbleGame._versus.inviteHint({ mode: dropAndFusionModeLabel(gameMode) }) }}</div>
							<div class="_buttonsCenter">
								<MkButton primary rounded @click="inviteVersus">{{ i18n.ts._mkgoBubbleGame._versus.inviteButton }}</MkButton>
								<MkButton rounded @click="openVersusHistory"><i class="ti ti-history"></i> {{ i18n.ts._mkgoBubbleGame._versus.history }}</MkButton>
							</div>
							<template v-if="versusInvitations.length > 0">
								<div><b>{{ i18n.ts._mkgoBubbleGame._versus.invitations }}</b></div>
								<div v-for="inv in versusInvitations" :key="inv.id" :class="$style.invitation">
									<MkAvatar v-if="inv.user1" :user="inv.user1" style="width: 28px; height: 28px; margin-right: 8px;"/>
									<div style="min-width: 0;">
										<div v-if="inv.user1"><MkUserName :user="inv.user1" :nowrap="true"/></div>
										<div style="font-size: 85%; opacity: 0.8;">{{ dropAndFusionModeLabel(inv.gameMode) }}</div>
									</div>
									<div style="margin-left: auto;" class="_buttons">
										<MkButton primary small rounded @click="acceptVersus(inv)">{{ i18n.ts._mkgoBubbleGame._versus.acceptInvitation }}</MkButton>
										<MkButton small rounded @click="declineVersus(inv)">{{ i18n.ts._mkgoBubbleGame._versus.declineInvitation }}</MkButton>
									</div>
								</div>
							</template>
						</div>
					</div>
				</div>
				<div class="_woodenFrame">
					<div class="_woodenFrameInner">
						<div class="_gaps_s" style="padding: 16px;">
							<div><b>{{ i18n.tsx.lastNDays({ n: 7 }) }} {{ i18n.ts.ranking }}</b> ({{ dropAndFusionModeLabel(gameMode) }})</div>
							<div v-if="ranking" class="_gaps_s">
								<div v-for="r in ranking" :key="r.id" :class="$style.rankingRecord">
									<MkAvatar v-if="r.user" :link="true" style="width: 24px; height: 24px; margin-right: 4px;" :user="r.user"/>
									<MkUserName v-if="r.user" :user="r.user" :nowrap="true"/>
									<b style="margin-left: auto;">{{ r.score.toLocaleString() }} {{ dropAndFusionScoreUnit(gameMode) }}</b>
								</div>
							</div>
							<div v-else>{{ i18n.ts.loading }}</div>
						</div>
					</div>
				</div>
				<div class="_woodenFrame">
					<div class="_woodenFrameInner" style="padding: 16px;">
						<div style="font-weight: bold;">{{ i18n.ts._bubbleGame.howToPlay }}</div>
						<ol>
							<li>{{ i18n.ts._bubbleGame._howToPlay.section1 }}</li>
							<li>{{ i18n.ts._bubbleGame._howToPlay.section2 }}</li>
							<li>{{ i18n.ts._bubbleGame._howToPlay.section3 }}</li>
						</ol>
					</div>
				</div>
				<div class="_woodenFrame">
					<div class="_woodenFrameInner">
						<div class="_gaps_s" style="padding: 16px;">
							<div><b>Credit</b></div>
							<div>
								<div>Ai-chan illustration: @poteriri@misskey.io</div>
								<div>BGM: @ys@misskey.design</div>
							</div>
						</div>
					</div>
				</div>
			</div>
		</div>
	</div>
	<XGame v-else :gameMode="gameMode" :mute="mute" :resume="resume" @end="onGameEnd"/>
</Transition>
</template>

<script lang="ts" setup>
import { computed, onActivated, onMounted, onUnmounted, ref, watch } from 'vue';
import * as Misskey from 'misskey-js';
import { DropAndFusionGame, gameModeOf } from 'misskey-bubble-game';
import XGame from './drop-and-fusion.game.vue';
import { definePage } from '@/page.js';
import MkButton from '@/components/MkButton.vue';
import { i18n } from '@/i18n.js';
import { useMkSelect } from '@/composables/use-mkselect.js';
import MkSelect from '@/components/MkSelect.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import { misskeyApi, misskeyApiGet } from '@/utility/misskey-api.js';
import * as os from '@/os.js';
import { clearDropAndFusionSave, isDropAndFusionSaveExpired, loadDropAndFusionSave } from '@/utility/drop-and-fusion-save.js';
import type { DropAndFusionSave } from '@/utility/drop-and-fusion-save.js';
import { dropAndFusionModeLabel, dropAndFusionScoreUnit } from '@/utility/drop-and-fusion-mode.js';
import { connectVersusInvitations } from '@/utility/bubble-versus.js';
import type { VersusConnection, VersusMatch } from '@/utility/bubble-versus.js';
import { useRouter } from '@/router.js';

const {
	model: baseMode,
	def: baseModeDef,
} = useMkSelect({
	items: [
		{ label: 'NORMAL', value: 'normal' },
		{ label: 'SQUARE', value: 'square' },
		{ label: 'YEN', value: 'yen' },
		{ label: 'SWEETS', value: 'sweets' },
		// mk-go (#3194): SPACE は本家がメニューから外していた。はみ出しの判定の猶予
		// (#3193) と、玉が箱の外へ出ないようにした壁で遊べるようになった。
		{ label: 'SPACE', value: 'space' },
	],
	initialValue: 'normal',
});

// mk-go (#3216): 物理を形と別に選ぶ。#3194 の BOUNCY は NORMAL × BOUNCY になった
// (モードの文字列は `bouncy` のままなので、これまでのランキングと記録が続く)。
const {
	model: physics,
	def: physicsDef,
} = useMkSelect({
	items: [
		{ label: 'DEFAULT', value: 'default' },
		{ label: 'BOUNCY', value: 'bouncy' },
		{ label: 'FRICTION', value: 'friction' },
	],
	initialValue: 'default',
});

const physicsCaption = computed(() => {
	switch (physics.value) {
		case 'bouncy': return i18n.ts._mkgoBubbleGame.physicsBouncy;
		case 'friction': return i18n.ts._mkgoBubbleGame.physicsFriction;
		default: return i18n.ts._mkgoBubbleGame.physicsDefault;
	}
});

// ランキング・ハイスコア・途中保存・スコアの登録は、すべてこの文字列で分かれる。
const gameMode = computed(() => gameModeOf(baseMode.value, physics.value));
const gameStarted = ref(false);
const mute = ref(false);
const ranking = ref<Misskey.entities.BubbleGameRankingResponse | null>(null);

watch(gameMode, async () => {
	ranking.value = await misskeyApiGet('bubble-game/ranking', { gameMode: gameMode.value });
}, { immediate: true });

// mk-go (#3192): 続きから遊ぶときの途中保存。
const resume = ref<DropAndFusionSave | null>(null);
// ダイアログを出している間に開始ボタンをもう一度押されても、二重に始めない。
let starting = false;

async function start() {
	if (starting) return;
	starting = true;
	try {
		await prepareStart();
	} finally {
		starting = false;
	}
}

async function prepareStart() {
	resume.value = null;

	const save = loadDropAndFusionSave(gameMode.value);
	if (save != null) {
		if (isDropAndFusionSaveExpired(save)) {
			// 古すぎる保存は再開しない。終えてもスコアを登録できない (backend は
			// 7 日より古いシードを受け付けない)。
			clearDropAndFusionSave(gameMode.value);
			await os.alert({
				type: 'info',
				text: i18n.ts._mkgoBubbleGame.saveDiscardedExpired,
			});
		} else if (save.v !== DropAndFusionGame.VERSION) {
			// **版が違う保存は再開しない。** エンジンが変わると同じ記録でも結果が
			// 変わるので、中断前とは別の盤面になる。
			clearDropAndFusionSave(gameMode.value);
			await os.alert({
				type: 'info',
				text: i18n.ts._mkgoBubbleGame.saveDiscardedVersion,
			});
		} else {
			const { canceled, result } = await os.actions({
				type: 'question',
				title: i18n.ts._mkgoBubbleGame.resumeTitle,
				text: i18n.ts._mkgoBubbleGame.resumeText,
				actions: [{
					value: 'resume',
					text: i18n.ts._mkgoBubbleGame.resumeContinue,
					primary: true,
				}, {
					value: 'new',
					text: i18n.ts._mkgoBubbleGame.resumeNew,
				}] as const,
			});
			if (canceled) return;
			if (result === 'resume') {
				resume.value = save;
			} else {
				clearDropAndFusionSave(gameMode.value);
			}
		}
	}

	gameStarted.value = true;
}

function onGameEnd() {
	gameStarted.value = false;
}

// mk-go (#3231): 対戦の招待。
const router = useRouter();
const versusInvitations = ref<VersusMatch[]>([]);
let versusConnection: VersusConnection | null = null;

async function fetchVersusInvitations() {
	try {
		const res = await misskeyApi('bubble-game/versus/invitations');
		versusInvitations.value = res;
	} catch {
		// 一覧が取れなくても一人で遊ぶのには関係ないので、黙って空のままにする。
	}
}

function openVersus(matchId: string) {
	router.push('/bubble-game/versus/:matchId', { params: { matchId } });
}

// mk-go (#3232): 自分の対戦の履歴。
function openVersusHistory() {
	router.push('/bubble-game/versus/history/:userId?', { params: {} });
}

async function inviteVersus() {
	const user = await os.selectUser({ includeSelf: false, localOnly: true });
	if (user == null) return;
	const match = await os.apiWithDialog('bubble-game/versus/invite', {
		userId: user.id,
		gameMode: gameMode.value,
	});
	openVersus(match.id);
}

async function acceptVersus(inv: VersusMatch) {
	await os.apiWithDialog('bubble-game/versus/accept', { matchId: inv.id });
	openVersus(inv.id);
}

async function declineVersus(inv: VersusMatch) {
	await os.apiWithDialog('bubble-game/versus/decline', { matchId: inv.id });
	versusInvitations.value = versusInvitations.value.filter(x => x.id !== inv.id);
}

onMounted(() => {
	fetchVersusInvitations();
	versusConnection = connectVersusInvitations();
	versusConnection.on('invited', (x: { user: { username: string; name: string | null } }) => {
		fetchVersusInvitations();
		os.toast(i18n.tsx._mkgoBubbleGame._versus.newInvitation({ name: x.user.name ?? x.user.username }));
	});
	// 取り消された招待は一覧から消す。
	versusConnection.on('canceled', () => { fetchVersusInvitations(); });
});

// ページはキャッシュされるので、戻ってきたら一覧を取り直す (受けた招待が残る)。
onActivated(() => {
	fetchVersusInvitations();
});

onUnmounted(() => {
	versusConnection?.dispose();
	versusConnection = null;
});

definePage(() => ({
	title: i18n.ts.bubbleGame,
	icon: 'ti ti-device-gamepad',
}));
</script>

<style lang="scss" module>
.transition_zoom_move,
.transition_zoom_enterActive,
.transition_zoom_leaveActive {
	transition: opacity 0.5s cubic-bezier(0,.5,.5,1), transform 0.5s cubic-bezier(0,.5,.5,1) !important;
}
.transition_zoom_enterFrom,
.transition_zoom_leaveTo {
	opacity: 0;
	transform: scale(0.8);
}

.root {
	margin: 0 auto;
	max-width: 600px;
	user-select: none;

	* {
		user-select: none;
	}
}

.invitation {
	display: flex;
	align-items: center;
	padding-top: 4px;
}

.rankingRecord {
	display: flex;
	line-height: 24px;
	padding-top: 4px;
	white-space: nowrap;
	overflow: visible;
	text-overflow: ellipsis;
}
</style>
