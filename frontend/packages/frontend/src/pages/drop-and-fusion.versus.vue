<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go (#3231): バブルゲームの 1:1 対戦 (#3228)。招待への返事・準備・対局・結果を
	この画面が受け持つ。盤面はそれぞれの端末が動かし、サーバーは攻撃の中継と終局の
	判定だけを行う。
-->
<template>
<div v-if="match == null" class="_spacer" style="--MI_SPACER-w: 600px;">
	<MkLoading v-if="loadFailed == null"/>
	<MkResult v-else-if="loadFailed === 'notFound'" type="notFound" :text="i18n.ts._mkgoBubbleGame._versus.notFound"/>
	<MkResult v-else type="error"/>
</div>
<div v-else-if="started != null && match.status !== 'ended'">
	<div class="_spacer" style="--MI_SPACER-w: 800px; padding-bottom: 0;">
		<div :class="$style.root">
			<div class="_woodenFrame">
				<div class="_woodenFrameInner" :class="$style.hud">
					<div :class="$style.hudInfo">
						<div :class="$style.hudTime"><i class="ti ti-clock"></i> {{ formatRemaining(remaining) }}</div>
						<MkButton v-if="pendingReport != null && !sendingReport" small rounded danger @click="sendReport">{{ i18n.ts._mkgoBubbleGame._versus.resendReport }}</MkButton>
						<div v-if="opponent" :class="$style.hudOpponent">
							<MkAvatar :user="opponent" :class="$style.hudAvatar"/>
							<span :class="$style.hudName"><MkUserName :user="opponent" :nowrap="true"/></span>
						</div>
						<div v-if="opponentState">
							<MkNumber :value="opponentState.score"/>
							<span v-if="opponentState.pending > 0" :class="$style.hudPending"> +{{ opponentState.pending }}</span>
							<i v-if="opponentState.danger" class="ti ti-alert-triangle" :class="$style.hudDanger"></i>
							<span v-if="opponentState.gameOver"> ({{ i18n.ts._mkgoBubbleGame._versus.opponentFinished }})</span>
						</div>
					</div>
					<XBoard :board="opponentState?.board" :danger="opponentState?.danger ?? false" :class="$style.hudBoard"/>
				</div>
			</div>
		</div>
	</div>
	<XGame
		ref="gameEl"
		:gameMode="match.gameMode"
		:mute="mute"
		:versus="versusProps"
		@versusAttack="onAttack"
		@versusFinished="onFinished"
		@end="onGameEnd"
	/>
</div>
<div v-else class="_spacer" style="--MI_SPACER-w: 600px;">
	<div :class="$style.root" class="_gaps">
		<div class="_woodenFrame">
			<div class="_woodenFrameInner" style="text-align: center;">
				<b>{{ i18n.ts._mkgoBubbleGame._versus.title }}</b>
				<div>- {{ dropAndFusionModeLabel(match.gameMode) }} -</div>
			</div>
		</div>

		<div class="_woodenFrame">
			<div class="_woodenFrameInner" :class="$style.players">
				<div v-for="(user, i) in [match.user1, match.user2]" :key="i" :class="$style.player">
					<MkAvatar v-if="user" :user="user" :class="$style.playerAvatar" :link="true"/>
					<div v-if="user"><MkUserName :user="user" :nowrap="true"/></div>
					<div v-if="match.status === 'accepted'" :class="$style.playerReady">
						<span v-if="(i === 0 ? match.user1Ready : match.user2Ready)" style="color: var(--MI_THEME-success);"><i class="ti ti-check"></i> {{ i18n.ts._mkgoBubbleGame._versus.ready }}</span>
						<span v-else style="opacity: 0.7;">{{ i18n.ts._mkgoBubbleGame._versus.notReady }}</span>
					</div>
					<div v-if="match.status === 'ended'" :class="$style.playerResult">
						<i v-if="match.winnerId != null && match.winnerId === (i === 0 ? match.user1Id : match.user2Id)" class="ti ti-trophy" style="color: var(--MI_THEME-accent);"></i>
						<span v-if="scoreOf(i) != null"><MkNumber :value="scoreOf(i)!"/></span>
					</div>
				</div>
			</div>
		</div>

		<div class="_woodenFrame">
			<div class="_woodenFrameInner">
				<div class="_gaps" style="padding: 8px; text-align: center;">
					<template v-if="match.status === 'invited'">
						<template v-if="mySide === 0">
							<div>{{ i18n.ts._mkgoBubbleGame._versus.waitingForAnswer }}<MkEllipsis/></div>
							<div class="_buttonsCenter">
								<MkButton rounded @click="cancel">{{ i18n.ts.cancel }}</MkButton>
							</div>
						</template>
						<template v-else>
							<div>{{ i18n.ts._mkgoBubbleGame._versus.invitedYou }}</div>
							<div class="_buttonsCenter">
								<MkButton primary rounded @click="accept">{{ i18n.ts._mkgoBubbleGame._versus.acceptInvitation }}</MkButton>
								<MkButton rounded @click="decline">{{ i18n.ts._mkgoBubbleGame._versus.declineInvitation }}</MkButton>
							</div>
						</template>
					</template>
					<template v-else-if="match.status === 'accepted'">
						<div>{{ i18n.ts._mkgoBubbleGame._versus.readyDescription }}</div>
						<div class="_buttonsCenter">
							<MkButton v-if="!myReady" primary gradate rounded @click="setReady(true)">{{ i18n.ts._mkgoBubbleGame._versus.ready }}</MkButton>
							<MkButton v-else rounded @click="setReady(false)">{{ i18n.ts._mkgoBubbleGame._versus.notReady }}</MkButton>
							<MkButton rounded danger @click="cancel">{{ i18n.ts._mkgoBubbleGame._versus.leave }}</MkButton>
						</div>
						<MkSwitch v-model="mute">
							<template #label>{{ i18n.ts.mute }}</template>
						</MkSwitch>
					</template>
					<template v-else-if="match.status === 'playing' && myResult != null">
						<div>{{ i18n.ts._mkgoBubbleGame._versus.waitingForOpponentResult }}<MkEllipsis/></div>
					</template>
					<template v-else-if="match.status === 'playing'">
						<!-- 読み込み直した・別の端末で開いた。盤面は端末にしか無いので再開できない。 -->
						<div>{{ i18n.ts._mkgoBubbleGame._versus.cannotResume }}</div>
						<div class="_buttonsCenter">
							<MkButton rounded danger @click="surrenderFromLobby">{{ i18n.ts.surrender }}</MkButton>
						</div>
					</template>
					<template v-else-if="match.status === 'ended'">
						<div :class="$style.outcome">{{ outcomeLabel }}</div>
						<div style="opacity: 0.8;">{{ reasonLabel }}</div>
						<div class="_buttonsCenter">
							<MkButton primary rounded @click="leave">{{ i18n.ts.backToTitle }}</MkButton>
							<MkButton rounded @click="openReplay">{{ i18n.ts._mkgoBubbleGame._versus.showReplay }}</MkButton>
						</div>
					</template>
				</div>
			</div>
		</div>
	</div>
</div>
</template>

<script lang="ts" setup>
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref, shallowRef, useTemplateRef } from 'vue';
import { DropAndFusionGame } from 'misskey-bubble-game';
import XGame from './drop-and-fusion.game.vue';
import XBoard from './drop-and-fusion.versus.board.vue';
import { definePage } from '@/page.js';
import MkButton from '@/components/MkButton.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import MkNumber from '@/components/MkNumber.vue';
import MkResult from '@/components/global/MkResult.vue';
import { i18n } from '@/i18n.js';
import { $i } from '@/i.js';
import * as os from '@/os.js';
import { useRouter } from '@/router.js';
import { useInterval } from '@@/js/use-interval.js';
import { useStream } from '@/stream.js';
import { dropAndFusionModeLabel } from '@/utility/drop-and-fusion-mode.js';
import { connectVersusMatch, versusApi } from '@/utility/bubble-versus.js';
import { versusOutcomeLabel, versusReasonLabel } from '@/utility/bubble-versus-labels.js';
import type { VersusBoardState, VersusConnection, VersusMatch, VersusReport, VersusStarted } from '@/utility/bubble-versus.js';
import {
	REPORT_RETRY_DELAYS_MS,
	VERSUS_STATE_INTERVAL_MS,
	formatRemaining,
	isRetryableReportError,
	opponentOf,
	outcomeFor,
	remainingMs,
	shouldClaimDisconnected,
	sideOf,
} from '@/utility/bubble-versus-rules.js';

const props = defineProps<{
	matchId: string;
}>();

const router = useRouter();

const match = ref<VersusMatch | null>(null);
const loadFailed = ref<'notFound' | 'error' | null>(null);
const mute = ref(false);
// 開始の知らせを受けた。startAtLocal はこの端末の時計で始める時刻。
const started = shallowRef<(VersusStarted & { startAtLocal: number }) | null>(null);
const opponentState = ref<VersusBoardState | null>(null);
const remaining = ref(0);
// 報告せずに終わったときの、この端末での最後の得点。
const localFinalScore = ref<number | null>(null);
// まだ届いていない報告。届かなかったときに画面から送り直す。
const pendingReport = shallowRef<VersusReport | null>(null);
const sendingReport = ref(false);
const gameEl = useTemplateRef('gameEl');

let connection: VersusConnection | null = null;
// 相手から最後に何か届いた時刻 (この端末の時計)。
let lastHeardFromOpponent = 0;
let reported = false;
let lastClaimAt = 0;
let lastStateAt = 0;

const mySide = computed(() => match.value == null ? -1 : sideOf(match.value, $i?.id));
const opponent = computed(() => match.value == null ? null : opponentOf(match.value, $i?.id));
const myReady = computed(() => {
	if (match.value == null) return false;
	return mySide.value === 0 ? match.value.user1Ready : match.value.user2Ready;
});
const myResult = computed(() => {
	if (match.value == null) return null;
	return mySide.value === 0 ? match.value.user1Result : mySide.value === 1 ? match.value.user2Result : null;
});
// インラインのオブジェクトで渡すと、残り時間が変わるたびにゲームの画面ごと描き直す。
const versusProps = computed(() => started.value == null ? null : { seed: started.value.seed, startAtLocal: started.value.startAtLocal });
const opponentId = computed(() => {
	if (match.value == null) return null;
	return mySide.value === 0 ? match.value.user2Id : match.value.user1Id;
});

function scoreOf(i: number): number | null {
	if (match.value == null) return null;
	const result = i === 0 ? match.value.user1Result : match.value.user2Result;
	if (result != null) return result.score;
	return i === mySide.value ? localFinalScore.value : null;
}

const outcomeLabel = computed(() => match.value == null ? '' : versusOutcomeLabel(outcomeFor(match.value, $i?.id)));

const reasonLabel = computed(() => versusReasonLabel(match.value?.reason));

async function fetchMatch() {
	try {
		match.value = await versusApi<VersusMatch>('show', { matchId: props.matchId });
	} catch (err: any) {
		// 一時的な失敗で「期限切れ」と出さない。取れていた対局はそのまま見せる。
		if (err?.code === 'NO_SUCH_MATCH') loadFailed.value = 'notFound';
		else if (match.value == null) loadFailed.value = 'error';
	}
}

function connect() {
	if (connection != null) return;
	connection = connectVersusMatch(props.matchId);
	connection.on('error', (x: { type: string; code: string }) => {
		// 申告と盤面の知らせの失敗は目安の送信なので黙って捨てる (早すぎれば断られるだけ)。
		if (x.type === 'claimDisconnected' || x.type === 'state') return;
		os.toast(i18n.ts._mkgoBubbleGame._versus.operationFailed);
	});
	connection.on('accepted', () => { fetchMatch(); });
	connection.on('readyStates', (x: { user1: boolean; user2: boolean }) => {
		if (match.value == null) return;
		match.value.user1Ready = x.user1;
		match.value.user2Ready = x.user2;
	});
	connection.on('started', (x: VersusStarted) => {
		if (match.value == null) return;
		// 端末の時計はサーバーとずれているので、受け取った時刻から数える。
		const startAtLocal = Date.now() + x.countdownMs;
		lastHeardFromOpponent = startAtLocal;
		match.value.status = 'playing';
		started.value = { ...x, startAtLocal };
	});
	connection.on('attack', (x: { from: string; count: number }) => {
		if (x.from !== opponentId.value) return;
		lastHeardFromOpponent = Date.now();
		gameEl.value?.receiveAttack(x.count);
	});
	connection.on('state', (x: { userId: string; state: VersusBoardState }) => {
		if (x.userId !== opponentId.value) return;
		lastHeardFromOpponent = Date.now();
		opponentState.value = x.state;
	});
	connection.on('ended', () => {
		// 相手が先に終わると、対局の画面はこの後の取り直しで閉じる。その前に
		// この盤面を止めて、記録を報告する (勝敗には効かない。リプレイに残すため、
		// #3232)。こちらが先に終わっていれば何もしない。得点は結果に出すために残す。
		if (gameEl.value != null) {
			localFinalScore.value = gameEl.value.boardState().score;
			gameEl.value.finishByOpponentEnded();
		}
		fetchMatch();
	});
	connection.on('declined', () => {
		connection?.dispose();
		connection = null;
		os.alert({ type: 'info', text: i18n.ts._mkgoBubbleGame._versus.declined });
		leave();
	});
	connection.on('canceled', (x: { userId: string }) => {
		if (x.userId === $i?.id) return;
		connection?.dispose();
		connection = null;
		os.alert({ type: 'info', text: i18n.ts._mkgoBubbleGame._versus.canceled });
		leave();
	});
}

useInterval(() => {
	const s = started.value;
	if (match.value == null || match.value.status === 'ended') return;
	const now = Date.now();
	if (s == null) {
		// 報告を済ませてから読み込み直した。盤面は無いが、相手が戻らなければ勝ちを
		// 申告できる (判定はサーバーがする)。
		if (match.value.status === 'playing' && myResult.value != null && now - lastClaimAt >= 5_000) {
			lastClaimAt = now;
			connection?.send('claimDisconnected', {});
		}
		return;
	}
	remaining.value = remainingMs(s.startAtLocal, s.timeLimitMs, now);
	if (now < s.startAtLocal) return;

	if (remaining.value === 0) {
		gameEl.value?.finishByTimeUp();
	}

	if (now - lastStateAt >= VERSUS_STATE_INTERVAL_MS && gameEl.value != null) {
		lastStateAt = now;
		connection?.send('state', gameEl.value.boardState());
	}

	// サーバーが最終的に判定するので、ここは目安で申告する (早すぎれば断られるだけ)。
	if (now - lastClaimAt >= 5_000 && shouldClaimDisconnected({
		now,
		lastHeardFromOpponent,
		localStartedAt: s.startAtLocal,
		timeLimitMs: s.timeLimitMs,
		reported,
	})) {
		lastClaimAt = now;
		connection?.send('claimDisconnected', {});
	}
// **画面が隠れていても止めない。** 既定の interval は非表示の間止まるので、別の
// アプリへ 30 秒切り替えただけで盤面の知らせが途切れて切断負けになり、時間切れの
// 報告も出なくなる (盤面は止まるが、止まった得点で報告できる)。
}, 500, { immediate: false, afterMounted: true, keepRunningWhenHidden: true });

function onAttack(count: number) {
	connection?.send('attack', { count });
}

async function onFinished(report: VersusReport) {
	pendingReport.value = report;
	await sendReport();
}

// 報告が届かないと、得点が上でも相手の申告で負ける。一時的な失敗はやり直し、
// 諦めたら画面からもう一度送れるようにする。
async function sendReport() {
	const report = pendingReport.value;
	if (report == null || sendingReport.value) return;
	sendingReport.value = true;
	try {
		for (let attempt = 0; attempt < REPORT_RETRY_DELAYS_MS.length + 1; attempt++) {
			try {
				match.value = await versusApi<VersusMatch>('report', { matchId: props.matchId, ...report });
				reported = true;
				pendingReport.value = null;
				return;
			} catch (err: any) {
				if (!isRetryableReportError(err?.code) || attempt === REPORT_RETRY_DELAYS_MS.length) break;
				await new Promise(resolve => window.setTimeout(resolve, REPORT_RETRY_DELAYS_MS[attempt]));
			}
		}
		// 相手の終局を受けての報告 (opponentEnded) は勝敗に効かず、終局後の画面には
		// 送り直す手段も無いので、「送り直してください」とは言わない (リプレイに
		// この盤面が出ないだけ)。
		if (report.reason === 'opponentEnded') {
			pendingReport.value = null;
			return;
		}
		os.toast(i18n.ts._mkgoBubbleGame._versus.reportFailed);
	} finally {
		sendingReport.value = false;
	}
}

async function accept() {
	match.value = await os.apiWithDialog('bubble-game/versus/accept' as never, { matchId: props.matchId } as never) as VersusMatch;
}

async function decline() {
	await os.apiWithDialog('bubble-game/versus/decline' as never, { matchId: props.matchId } as never);
	leave();
}

async function cancel() {
	await os.apiWithDialog('bubble-game/versus/cancel' as never, { matchId: props.matchId } as never);
	leave();
}

function setReady(ready: boolean) {
	connection?.send('ready', ready);
}

async function surrenderFromLobby() {
	const { canceled } = await os.confirm({ type: 'warning', text: i18n.ts.areYouSure });
	if (canceled) return;
	match.value = await os.apiWithDialog('bubble-game/versus/report' as never, {
		matchId: props.matchId,
		score: 0,
		frame: 0,
		reason: 'surrender',
		logs: [],
		// mk-go (#3232): 記録は空でも版は付ける (付けないと「別の版」と区別できない)。
		gameVersion: DropAndFusionGame.VERSION,
	} as never) as VersusMatch;
}

function leave() {
	router.push('/bubble-game');
}

// mk-go (#3232): 記録は両者の報告が届いてから並ぶので、終局の直後は片方の盤面しか
// 無いことがある (リプレイの画面がその盤面を「記録が無い」と出す)。
function openReplay() {
	router.push('/bubble-game/versus/replay/:matchId', { params: { matchId: props.matchId } });
}

function onGameEnd() {
	// 対局の画面は、離れて戻ると 'end' を出す (盤面はもう片付いている)。対局が
	// 続いているなら、タイトルへ飛ばさずに「再開できない」の表示に倒す。
	if (match.value != null && match.value.status !== 'ended') {
		started.value = null;
		return;
	}
	leave();
}

let active = false;

async function activate() {
	active = true;
	await fetchMatch();
	// 取りに行っている間に離れた。つなぐと購読が残る。
	if (!active) return;
	if (match.value == null || match.value.status === 'ended') return;
	connect();
	// show とつなぐ間に届いた知らせ (受諾・開始) を取りこぼさないよう、つないだ後に
	// もう一度取る。
	await fetchMatch();
}

function deactivate() {
	active = false;
	connection?.dispose();
	connection = null;
}

// 再接続の間に流れた知らせは届かないので、つなぎ直したら取り直す。開始を
// 取りこぼした場合は「再開できない」の表示になる。
function onStreamConnected() {
	if (active) fetchMatch();
}

onMounted(() => {
	useStream().on('_connected_', onStreamConnected);
	activate();
});

// **離れたら購読を切る。** ページはキャッシュされる (KeepAlive) ので、残すと
// 別のページを見ている間に断られた知らせでタイトルへ引き戻す。
onDeactivated(deactivate);

onActivated(() => {
	if (!active) activate();
});

onUnmounted(() => {
	useStream().off('_connected_', onStreamConnected);
	deactivate();
});

definePage(() => ({
	title: i18n.ts._mkgoBubbleGame._versus.title,
	icon: 'ti ti-device-gamepad',
}));
</script>

<style lang="scss" module>
.root {
	margin: 0 auto;
	max-width: 600px;
	user-select: none;
}

.hud {
	display: flex;
	align-items: flex-start;
	gap: 12px;
}

.hudInfo {
	display: flex;
	flex-direction: column;
	gap: 4px;
	min-width: 0;
	flex: 1;
}

.hudBoard {
	flex-shrink: 0;
}

.hudTime {
	font-weight: bold;
	font-variant-numeric: tabular-nums;
}

.hudOpponent {
	display: flex;
	align-items: center;
	gap: 6px;
	min-width: 0;
}

.hudAvatar {
	width: 24px;
	height: 24px;
}

.hudName {
	max-width: 8em;
	overflow: hidden;
}

.hudPending {
	color: var(--MI_THEME-warn);
}

.hudDanger {
	color: var(--MI_THEME-error);
}

.players {
	display: flex;
	justify-content: space-around;
	padding: 8px;
}

.player {
	display: flex;
	flex-direction: column;
	align-items: center;
	gap: 4px;
	min-width: 0;
}

.playerAvatar {
	width: 48px;
	height: 48px;
}

.playerReady,
.playerResult {
	font-size: 90%;
}

.outcome {
	font-size: 1.6em;
	font-weight: bold;
}
</style>
