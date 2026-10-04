<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div class="_gaps_m">
	<!--
		**確認できなかったことを隠さない (#2961)。** 0 件として描くと、実際には
		申請があるユーザーを「履歴なし」と判断する。審査画面が nameConflict /
		remoteGone で採っているのと同じ判断。
	-->
	<!--
		**集計にも再試行を置く (レビュー M1)。** 失敗すると期間別の使用状況ごと
		消えるうえ、`fetchSummary` の呼び出しは onMounted の 1 箇所しか無いので、
		復旧手段がページのリロードだけになる (#2960 が mk.20c で直したのと同じ形)。
		文面が「もう一度読み込んでください」と指示しているのに、その操作が UI に
		無い状態でもあった。
	-->
	<div v-if="summaryFailed" class="_gaps_s">
		<MkInfo warn>{{ i18n.ts._emojiApplication.summaryUnknown }}</MkInfo>
		<MkButton :disabled="summaryFetching" @click="fetchSummary">{{ i18n.ts.retry }}</MkButton>
	</div>
	<FormSection v-else-if="counts" :first="true">
		<template #label>{{ i18n.ts._emojiApplication.userSummaryTitle }}</template>
		<div :class="$style.counts">
			<div v-for="c in countCards" :key="c.key" :class="$style.count">
				<div :class="$style.countValue">{{ c.value }}</div>
				<div :class="$style.countLabel">{{ c.label }}</div>
			</div>
		</div>
	</FormSection>

	<!--
		**審査待ちの上限は期間の窓とは独立 (レビュー L-3)。** 窓の節の中に置くと、
		窓の一覧が空になる構成で審査待ちが満杯でも行ごと消える。**上限が無いときは
		出さない (レビュー L-4)** — 「審査待ちの上限: 2件（上限なし）」は件数カードと
		同じ数字をラベルと噛み合わない形で重複させるだけ。
	-->
	<FormSection v-if="!summaryFailed && pendingLimit && !pendingLimit.unlimited">
		<template #label>{{ i18n.ts._emojiApplication.pendingLimitTitle }}</template>
		<div class="_gaps_s">
			<MkKeyValue oneline>
				<template #key>{{ i18n.ts._emojiApplication.pendingLimitTitle }}</template>
				<template #value>
					<span :class="quotaIsFull(pendingLimit) ? $style.full : undefined">{{ quotaUsageLabel(pendingLimit) }}</span>
				</template>
			</MkKeyValue>
			<!--
				**期間の窓が満杯のときは出さない (レビュー L-2)。** backend は両方
				満杯なら窓のエラーを優先する — 期間上限は全ステータスを数えるので
				「取り下げれば出せる」は嘘になる。画面でその抑制を打ち消さない。
			-->
			<MkInfo v-if="quotaIsFull(pendingLimit) && !anyWindowFull" warn>{{ i18n.ts._emojiApplication.pendingLimitFull }}</MkInfo>
		</div>
	</FormSection>

	<FormSection v-if="!summaryFailed && windows.length > 0">
		<template #label>{{ i18n.ts._emojiApplication.quotaTitle }}</template>
		<div class="_gaps_s">
			<MkKeyValue v-for="w in windows" :key="w.period" oneline>
				<template #key>{{ quotaPeriodLabel(w.period) }}</template>
				<template #value>
					<span :class="quotaIsFull(w) ? $style.full : undefined">{{ quotaUsageLabel(w) }}</span>
				</template>
			</MkKeyValue>
			<!--
				**上限に達しているときだけ次に出せる日時を出す。** 空きがあるのに
				出すと「今は出せない」と読める。時刻が無い場合 (審査待ちの上限も
				同時に満杯) は時刻を騙らない。
			-->
			<template v-for="w in windows" :key="`full-${w.period}`">
				<MkInfo v-if="quotaIsFull(w)" warn>
					{{ w.retryAt ? i18n.tsx._emojiApplication.quotaFullUntil({ retryAt: formatDateTime(w.retryAt) }) : i18n.ts._emojiApplication.quotaUnknownRetry }}
				</MkInfo>
			</template>
		</div>
	</FormSection>

	<!--
		**枠のリセット (#2962)。** 期間上限の節の後ろに置く — 「何件使っているか」を
		見てから押す操作なので、上限の表示より前にあると判断材料が後から来る。
		**ボタンは、すべての期間上限が無制限なら出さない** (戻す枠が無い)。
		**節そのものは履歴があれば出す** — 上限を撤廃したあとに「最後のリセット」を
		確認できなくなるため (issue が非表示を求めたのはボタン)。
	-->
	<FormSection v-if="!summaryFailed && (canResetQuota(windows) || lastReset)">
		<template #label>{{ i18n.ts._emojiApplication.resetQuotaTitle }}</template>
		<div class="_gaps_s">
			<MkKeyValue oneline>
				<template #key>{{ i18n.ts._emojiApplication.lastReset }}</template>
				<template #value>
					<MkTime v-if="lastReset" :time="lastReset.at" mode="detail"/>
					<span v-else>{{ i18n.ts._emojiApplication.lastResetNone }}</span>
				</template>
			</MkKeyValue>
			<template v-if="lastReset">
				<MkKeyValue oneline>
					<template #key>{{ i18n.ts._emojiApplication.lastResetBy }}</template>
					<template #value><MkA :to="`/admin/user/${lastReset.byId}`" class="_link">{{ lastReset.byId }}</MkA></template>
				</MkKeyValue>
				<MkKeyValue oneline>
					<template #key>{{ i18n.ts._emojiApplication.resetQuotaReason }}</template>
					<template #value>{{ lastReset.reason }}</template>
				</MkKeyValue>
			</template>
			<!--
				**何が起きて何が起きないかを押す前に出す。** 履歴が消えると思って
				押されると取り返しがつかないし、短時間の送信制限と審査待ちの上限は
				これでは解除されない (審査待ちは処理しない限り減らない)。
			-->
			<MkInfo>{{ i18n.ts._emojiApplication.resetQuotaNote }}</MkInfo>
			<!--
				**ボタンだけを隠す (レビュー L4)。** 節ごと消すと、上限を撤廃した
				あとに「最後のリセット」を確認できなくなる (issue が非表示を
				求めたのはボタン)。
			-->
			<MkButton v-if="canResetQuota(windows)" :disabled="resetting" danger @click="resetQuota">{{ i18n.ts._emojiApplication.resetQuota }}</MkButton>
		</div>
	</FormSection>

	<FormSection>
		<template #label>{{ i18n.ts._emojiApplication.userHistoryTitle }}</template>
		<div class="_gaps_s">
			<div :class="$style.filters">
				<MkSelect v-model="status" :items="statusDef" :class="$style.filter" @update:modelValue="reload"/>
				<MkInput v-model="query" :class="$style.filter" type="search" :debounce="true" @update:modelValue="reload">
					<template #prefix><i class="ti ti-search"></i></template>
					<template #label>{{ i18n.ts._emojiApplication.searchPlaceholder }}</template>
				</MkInput>
			</div>

			<MkInfo v-if="historyFailed && items.length === 0" warn>{{ i18n.ts._emojiApplication.historyUnknown }}</MkInfo>
			<MkLoading v-else-if="fetching && items.length === 0"/>
			<div v-else-if="items.length === 0" :class="$style.empty">{{ i18n.ts._emojiApplication.noApplications }}</div>

			<div v-for="item in items" :key="item.id" :class="$style.row">
				<div :class="$style.thumb">
					<img v-if="previewUrls.get(item.id)" :src="previewUrls.get(item.id)!" :alt="item.name" :class="$style.thumbImg" @error="onPreviewError(item)"/>
					<span v-else :class="$style.thumbGone">{{ imageMissingLabel(item) }}</span>
				</div>
				<div :class="$style.body">
					<div :class="$style.head">
						<i :class="item.remoteHost ? 'ti ti-world-download' : 'ti ti-mood-smile'"></i>
						<span class="_monospace">:{{ item.name }}:</span>
						<span :class="[$style.status, $style[item.status]]">{{ relatedStatusLabel(item.status) }}</span>
					</div>
					<MkKeyValue v-if="item.remoteHost" oneline>
						<template #key>{{ i18n.ts._emojiApplication.remoteSource }}</template>
						<template #value><span class="_monospace">:{{ item.remoteName }}:@{{ item.remoteHost }}</span></template>
					</MkKeyValue>
					<MkKeyValue oneline>
						<template #key>{{ i18n.ts._emojiApplication.appliedAt }}</template>
						<template #value><MkTime :time="item.createdAt" mode="detail"/></template>
					</MkKeyValue>
					<MkKeyValue v-if="item.processedAt" oneline>
						<template #key>{{ i18n.ts._emojiApplication.processedAt }}</template>
						<template #value><MkTime :time="item.processedAt" mode="detail"/></template>
					</MkKeyValue>
					<MkKeyValue v-if="item.emojiId" oneline>
						<template #key>{{ i18n.ts._emojiApplication.emojiId }}</template>
						<template #value><span class="_monospace">{{ item.emojiId }}</span></template>
					</MkKeyValue>
					<!-- **却下理由が本体。** これが無いと「却下された」しか分からない。 -->
					<MkInfo v-if="item.rejectReason" warn>{{ item.rejectReason }}</MkInfo>
				</div>
			</div>

			<!--
				**追加読み込みに失敗しても、読めていた履歴は消さない。** 確認できて
				いたものまで隠すと、判断材料が減る方向に倒れる。
			-->
			<MkInfo v-if="historyFailed && items.length > 0" warn>{{ i18n.ts._emojiApplication.historyUnknown }}</MkInfo>
			<MkButton v-if="canLoadMore || historyFailed" :disabled="fetching" @click="loadMore">
				{{ historyFailed ? i18n.ts.retry : i18n.ts.loadMore }}
			</MkButton>
		</div>
	</FormSection>
</div>
</template>

<script lang="ts" setup>
import { computed, onMounted, ref } from 'vue';
import MkButton from '@/components/MkButton.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkInput from '@/components/MkInput.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkLoading from '@/components/global/MkLoading.vue';
import MkTime from '@/components/global/MkTime.vue';
import FormSection from '@/components/form/section.vue';
import { useMkSelect } from '@/composables/use-mkselect.js';
import { i18n } from '@/i18n.js';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { dateTimeFormat } from '@/utility/intl-const.js';
import { relatedImageMissingReason, relatedPreviewUrl, relatedStatusLabel } from '@/utility/emoji-application-related.js';
import { canLoadMoreUserApplications, canResetQuota, isValidResetReason, quotaIsFull, quotaPeriodLabel, quotaUsageLabel, userApplicationNextCursor } from '@/utility/emoji-application-user.js';
import type { PendingLimitView, QuotaResetView, QuotaWindowView } from '@/utility/emoji-application-user.js';

type Item = {
	id: string;
	status: 'pending' | 'approved' | 'rejected' | 'canceled';
	name: string;
	createdAt: string;
	processedAt: string | null;
	rejectReason?: string;
	emojiId?: string;
	url: string | null;
	remoteHost?: string;
	remoteName?: string;
};

type Counts = {
	total: number;
	pending: number;
	approved: number;
	rejected: number;
	canceled: number;
};

const props = defineProps<{ userId: string }>();

const PAGE = 30;

const counts = ref<Counts | null>(null);
const windows = ref<QuotaWindowView[]>([]);
const pendingLimit = ref<PendingLimitView | null>(null);
const lastReset = ref<QuotaResetView | null>(null);
const resetting = ref(false);
const items = ref<Item[]>([]);
const { model: status, def: statusDef } = useMkSelect({
	items: [
		{ label: i18n.ts._emojiApplication.filterAll, value: 'all' },
		{ label: i18n.ts._emojiApplication.statusPending, value: 'pending' },
		{ label: i18n.ts._emojiApplication.statusApproved, value: 'approved' },
		{ label: i18n.ts._emojiApplication.statusRejected, value: 'rejected' },
		{ label: i18n.ts._emojiApplication.statusCanceled, value: 'canceled' },
	],
	initialValue: 'all',
});
const query = ref('');
const fetching = ref(false);
const summaryFailed = ref(false);
const summaryFetching = ref(false);
const historyFailed = ref(false);
const lastPageSize = ref(0);
const brokenPreviews = ref(new Set<string>());

const canLoadMore = computed(() => canLoadMoreUserApplications(lastPageSize.value, PAGE));

const anyWindowFull = computed(() => windows.value.some(w => quotaIsFull(w)));

const countCards = computed(() => {
	const c = counts.value;
	if (c == null) return [];
	return [
		{ key: 'total', label: i18n.ts.total, value: c.total },
		{ key: 'pending', label: i18n.ts._emojiApplication.statusPending, value: c.pending },
		{ key: 'approved', label: i18n.ts._emojiApplication.statusApproved, value: c.approved },
		{ key: 'rejected', label: i18n.ts._emojiApplication.statusRejected, value: c.rejected },
		{ key: 'canceled', label: i18n.ts._emojiApplication.statusCanceled, value: c.canceled },
	];
});

// **1 行につき 1 回だけ解決する。** template から関数を呼ぶと再描画のたびに
// 走り、`<img>` の src が同値でも別インスタンスになる (審査画面と同じ理由)。
const previewUrls = computed(() => {
	const map = new Map<string, string | null>();
	for (const item of items.value) {
		map.set(item.id, relatedPreviewUrl(item, brokenPreviews.value));
	}
	return map;
});

// **この画面の文面にする (レビュー L1)。** 審査画面の `imageUnknown` は
// 「承認する前にもう一度読み込んでください」まで言うが、ここには承認操作が
// 無く、行の大半は処理済み。判定は共有したまま文面だけ分ける。
function imageMissingLabel(item: Item): string {
	return relatedImageMissingReason(item, brokenPreviews.value) === 'gone'
		? i18n.ts._emojiApplication.imageGone
		: i18n.ts._emojiApplication.imageUnknownShort;
}

function onPreviewError(item: Item) {
	brokenPreviews.value = new Set(brokenPreviews.value).add(item.id);
}

function formatDateTime(at: string): string {
	return dateTimeFormat.format(new Date(at));
}

async function fetchSummary() {
	if (summaryFetching.value) return;
	summaryFetching.value = true;
	try {
		const res = await misskeyApi('admin/emoji-application/user-summary' as never, {
			userId: props.userId,
		} as never) as unknown as { counts: Counts; windows: QuotaWindowView[]; pending: PendingLimitView; lastReset: QuotaResetView | null };
		counts.value = res.counts;
		windows.value = res.windows;
		pendingLimit.value = res.pending;
		lastReset.value = res.lastReset;
		summaryFailed.value = false;
	} catch {
		// **握り潰さない。** 0 件として描くと「申請なし」と読める。
		summaryFailed.value = true;
	} finally {
		summaryFetching.value = false;
	}
}

// **世代で古い応答を捨てる (レビュー H1)。** `if (fetching) return` で新しい
// 要求を捨てる形だと、取得中に絞り込みや検索を変えたときに**要求が 1 本も出ない
// まま、あとから解決した旧フィルタの結果が並ぶ**。「却下」と表示された一覧に
// 承認済みが混ざり、エラーもスピナーも出ないので気付けない (実測)。しかも次の
// 「もっと見る」は別の結果集合から採ったカーソルを渡すので、以降の行が永久に
// 出てこない。捨てるのは要求ではなく**古い応答**のほうにする。
let generation = 0;

async function fetchPage(untilId?: string) {
	const gen = ++generation;
	fetching.value = true;
	// **取り直す間は失敗の表示を出さない (レビュー M-3)。** 残すと、読み込み中も
	// 「確認できませんでした」が出たままになる (`v-else-if` なので読み込み中の
	// 表示に来ない)。`reload()` だけで戻していたが、いちばん押される復旧経路は
	// 再試行ボタン (`loadMore`) のほうだった。失敗したら catch が立て直す。
	historyFailed.value = false;
	try {
		const res = await misskeyApi('admin/emoji-application/list-by-user' as never, {
			userId: props.userId,
			status: status.value,
			query: query.value,
			limit: PAGE,
			untilId: untilId ?? null,
		} as never) as unknown as { items: Item[] };
		if (gen !== generation) return;
		items.value = untilId == null ? res.items : [...items.value, ...res.items];
		lastPageSize.value = res.items.length;
		historyFailed.value = false;
	} catch {
		if (gen !== generation) return;
		historyFailed.value = true;
	} finally {
		// **最新の要求だけが解除する。** 古い応答が解除すると、実際には
		// まだ飛んでいるのにボタンが押せる状態になる。
		if (gen === generation) fetching.value = false;
	}
}

// **理由を必ず取ってから送る (#2962)。** 監査ログに残る唯一の文脈なので、
// 空のまま押せる導線を作らない。server も同じ判定で 400 を返す。
async function resetQuota() {
	if (resetting.value) return;
	const { canceled, result: reason } = await os.inputText({
		title: i18n.ts._emojiApplication.resetQuotaTitle,
		text: i18n.ts._emojiApplication.resetQuotaNote,
		placeholder: i18n.ts._emojiApplication.resetQuotaReasonCaption,
		minLength: 1,
		// **列の長さに合わせる (レビュー M1)。** 超えると server が 400 を返すが、
		// 打ち終わってから弾かれるより入力の時点で止めるほうが早い。
		maxLength: 1024,
	});
	if (canceled) return;
	if (!isValidResetReason(reason)) {
		await os.alert({ type: 'warning', text: i18n.ts._emojiApplication.resetQuotaReasonRequired });
		return;
	}

	resetting.value = true;
	try {
		await misskeyApi('admin/emoji-application/reset-user-quota' as never, {
			userId: props.userId,
			reason,
		} as never);
		// **取り直す。** 枠が戻ったかどうかは件数の表示でしか確かめられない。
		// 返り値の lastReset だけ入れて件数を古いままにすると、押したのに
		// 「5 / 5」のままに見える。
		await fetchSummary();
		await os.alert({ type: 'success', text: i18n.ts._emojiApplication.resetQuotaDone });
	} catch {
		// **握り潰さない。** 成功したように見えると、戻っていない枠を戻ったものと
		// して扱うことになる。
		await os.alert({ type: 'error', text: i18n.ts._emojiApplication.resetQuotaFailed });
	} finally {
		resetting.value = false;
	}
}

function reload() {
	// 絞り込みを変えたら 1 ページ目から取り直す。**古い行を残さない** —
	// 残すと「却下だけ」を選んでいるのに承認済みが並ぶ。
	items.value = [];
	lastPageSize.value = 0;
	void fetchPage();
}

function loadMore() {
	// **1 件も読めていなければ最初から、読めていれば続きから取り直す
	// (レビュー R3-L2)。** 初回の失敗では `items` が空なのでカーソルは
	// undefined になり 1 ページ目を取る。2 ページ目以降の失敗では最後の
	// カーソルから続きを取る (先頭に戻すと読めていた行を取り直すだけになる)。
	void fetchPage(userApplicationNextCursor(items.value));
}

onMounted(() => {
	void fetchSummary();
	void fetchPage();
});
</script>

<style lang="scss" module>
.counts {
	display: grid;
	grid-template-columns: repeat(auto-fit, minmax(92px, 1fr));
	gap: 8px;
}

.count {
	padding: 10px;
	text-align: center;
	border: solid 1px var(--MI_THEME-divider);
	border-radius: var(--MI-radius-sm);
}

.countValue {
	font-size: 1.2em;
	font-weight: bold;
}

.countLabel {
	font-size: 0.85em;
	opacity: 0.7;
}

.full {
	color: var(--MI_THEME-warn);
	font-weight: bold;
}

.filters {
	display: flex;
	flex-wrap: wrap;
	gap: 8px;
}

.filter {
	flex: 1 1 180px;
	margin: 0;
}

.empty {
	padding: 16px;
	text-align: center;
	opacity: 0.7;
}

.row {
	display: grid;
	grid-template-columns: 64px 1fr;
	gap: 12px;
	align-items: start;
	padding: 10px;
	border: solid 1px var(--MI_THEME-divider);
	border-radius: var(--MI-radius-sm);
}

.thumb {
	display: flex;
	align-items: center;
	justify-content: center;
	min-height: 64px;
}

.thumbImg {
	max-width: 64px;
	max-height: 64px;
	object-fit: contain;
}

.thumbGone {
	font-size: 0.75em;
	opacity: 0.7;
	text-align: center;
}

.body {
	display: flex;
	flex-direction: column;
	gap: 6px;
	min-width: 0;
}

.head {
	display: flex;
	align-items: center;
	gap: 6px;
	flex-wrap: wrap;
}

.status {
	font-size: 0.8em;
	padding: 2px 6px;
	border-radius: var(--MI-radius-xs);
	background: var(--MI_THEME-buttonBg);
}

.pending {
	background: var(--MI_THEME-infoBg);
}

.approved {
	background: var(--MI_THEME-success);
	color: #fff;
}

.rejected {
	background: var(--MI_THEME-error);
	color: #fff;
}

.canceled {
	opacity: 0.7;
}
</style>
