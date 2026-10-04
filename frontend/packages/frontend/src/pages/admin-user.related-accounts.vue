<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: 対象ユーザーと同じ IP を使ったローカルアカウントの候補 (#3105、親 #3066)。

	**出るのは調査の候補であって判定ではない。** 同じ IP を使ったことは同一人物で
	あることを意味しない (家庭・職場・学校・公衆 Wi-Fi・携帯回線の CGNAT・VPN)。
	注意書きは結果の有無に関わらず常設する。

	**自動では引かない。** 機微な照会なので、押されたときだけ実行する。
-->
<template>
<div class="_gaps_m">
	<MkInfo>{{ i18n.ts._mkgoIpRelated.disclaimer }}</MkInfo>

	<div class="_gaps_s">
		<MkSelect v-model="sinceDays" :items="periodDef">
			<template #label>{{ i18n.ts._mkgoIpSearch.period }}</template>
			<template #caption>
				{{ i18n.ts._mkgoIpRelated.periodMeaning }}
				<template v-if="retentionDays != null">{{ i18n.tsx._mkgoIpSearch.retentionNote({ n: retentionDays }) }}</template>
			</template>
		</MkSelect>
		<MkButton primary :disabled="loading" @click="search(0)"><i class="ti ti-search"></i> {{ i18n.ts._mkgoIpRelated.run }}</MkButton>
	</div>

	<!-- 読み上げ用。常設しないと初回の結果が読まれない。 -->
	<div :class="$style.status" aria-live="polite">{{ status }}</div>

	<MkInfo v-if="error && !errorWhilePaging" warn>{{ error }}</MkInfo>

	<div v-if="loading" :class="$style.placeholder"><MkLoading/></div>

	<template v-else-if="result">
		<MkInfo v-if="notice === 'loggingDisabled'" warn>{{ i18n.ts._mkgoIpSearch.loggingDisabled }}</MkInfo>
		<MkInfo v-else-if="notice === 'loggingDisabledNoHistory'" warn>{{ i18n.ts._mkgoIpSearch.loggingDisabledNoHistory }}</MkInfo>
		<MkInfo v-else-if="notice === 'noHistory'" warn>{{ i18n.tsx._mkgoIpSearch.noHistory({ n: result.retentionDays }) }}</MkInfo>

		<!--
			**打ち切ったことを黙らない。** 立っているときは順位もスコアも下限で、
			続きを見れば順位が入れ替わりうる。
		-->
		<!--
			**両方立つことがあるので独立して出す。** `v-else-if` にすると、起点と
			候補の両方を切ったときに候補側の説明 (「集めていない候補のほうが強い
			可能性がある」「アカウント数も『以上』になる」「ページを送っても
			解消しない」) が丸ごと落ちる。IPv6 の起点 50 本超と CGNAT の IP は
			同時に起きやすい。
		-->
		<MkInfo v-if="result.targetIpsTruncated" warn>{{ i18n.tsx._mkgoIpRelated.targetIpsTruncated({ n: number(result.targetIpCount) }) }}</MkInfo>
		<MkInfo v-if="result.candidatesTruncated" warn>{{ i18n.ts._mkgoIpRelated.candidatesTruncated }}</MkInfo>

		<MkKeyValue oneline>
			<template #key>{{ i18n.ts._mkgoIpRelated.targetIpCount }}</template>
			<template #value>
				{{ i18n.tsx._mkgoIpRelated.targetIpCountValue({ n: number(result.targetIpCount) }) }}
				<span v-if="result.targetIpsTruncated">{{ i18n.ts._mkgoIpRelated.capped }}</span>
			</template>
		</MkKeyValue>
		<MkKeyValue oneline>
			<template #key>{{ i18n.ts._mkgoIpSearch.period }}</template>
			<template #value>{{ i18n.tsx._mkgoIpSearch.periodDays({ n: result.sinceDays }) }}</template>
		</MkKeyValue>

		<!--
			**「照合していない」と「照合したが一致が無い」を言い分ける。**
			対象の IP 記録が窓の中に 1 件も無ければ、候補側は 1 回も引いていない。
			打ち切ったときも同じで、見たのは全体の一部なので断定できない。
		-->
		<MkInfo v-if="outcome === 'noTargetRecords'" warn>{{ i18n.tsx._mkgoIpRelated.noTargetRecords({ n: result.sinceDays }) }}</MkInfo>
		<MkInfo v-else-if="outcome === 'noneOnThisPage'">{{ i18n.ts._mkgoIpSearch.noneOnThisPage }}</MkInfo>
		<MkInfo v-else-if="outcome === 'noneResolvable'">{{ i18n.ts._mkgoIpRelated.noneResolvable }}</MkInfo>
		<MkInfo v-else-if="outcome === 'noneResolvablePartial'">{{ i18n.ts._mkgoIpRelated.noneResolvablePartial }}</MkInfo>
		<MkInfo v-else-if="outcome === 'partial'">{{ i18n.ts._mkgoIpRelated.noneInSearchedRange }}</MkInfo>
		<MkInfo v-else-if="outcome === 'noMatch'">{{ i18n.ts._mkgoIpRelated.noMatch }}</MkInfo>
		<MkInfo v-else-if="outcome === 'noMatchInPeriod'">{{ i18n.ts._mkgoIpRelated.noMatchInPeriod }}</MkInfo>

		<div v-if="candidates.length > 0" class="_gaps_s">
			<div v-for="(c, i) in candidates" :key="c.user.id" :class="$style.row">
				<!--
					**順位は出すが、スコアは出さない。** 重みの単純和なので上限は 1 では
					なく、パーセントとして読めない。ここで数値を出すと「関連度 87%」の
					ように読まれる (#3105)。読む側が使うのは下の根拠のほう。
				-->
				<div :class="$style.rank">{{ i18n.tsx._mkgoIpRelated.displayOrder({ n: displayOrder(i) }) }}</div>
				<MkA :to="`/admin/user/${c.user.id}`" :class="$style.card">
					<MkUserCardMini :user="c.user" :withChart="false"/>
				</MkA>
				<div :class="$style.meta">
					<span v-if="c.isSuspended" :class="[$style.badge, $style.danger]">{{ i18n.ts._mkgoIpSearch.suspended }}</span>
					<span v-if="c.isDeleted" :class="[$style.badge, $style.danger]">{{ i18n.ts._mkgoIpSearch.deleted }}</span>
				</div>
				<MkKeyValue oneline>
					<template #key>{{ i18n.ts.lastActiveDate }}</template>
					<template #value>
						<MkTime v-if="c.lastActiveDate != null" :time="c.lastActiveDate" mode="detail"/>
						<span v-else>{{ i18n.ts._mkgoIpSearch.lastActiveUnknown }}</span>
					</template>
				</MkKeyValue>
				<MkKeyValue oneline>
					<template #key>{{ i18n.ts._mkgoIpRelated.sharedIpCount }}</template>
					<template #value>{{ i18n.tsx._mkgoIpRelated.sharedIpCountValue({ n: number(c.sharedIpCount) }) }}</template>
				</MkKeyValue>

				<!--
					**根拠。** 双方の最終観測をそれぞれ出す — 一致していなければ
					ただ両方の時刻が並ぶだけで、「同時に使っていた」ことは意味しない。
				-->
				<div :class="$style.evidence">
					<div v-for="p in c.sharedIps" :key="p.ip" :class="$style.match">
						<div class="_monospace">{{ p.ip }}</div>
						<div :class="$style.matchFacts">
							<span>{{ i18n.ts._mkgoIpRelated.targetLastSeen }}: <MkTime :time="p.targetLastSeenAt" mode="detail"/></span>
							<span>{{ i18n.ts._mkgoIpRelated.candidateLastSeen }}: <MkTime :time="p.candidateLastSeenAt" mode="detail"/></span>
							<span>
								{{ p.ipAccountCountIsLowerBound
									? i18n.tsx._mkgoIpRelated.ipAccountCountAtLeast({ n: number(p.ipAccountCount) })
									: i18n.tsx._mkgoIpRelated.ipAccountCount({ n: number(p.ipAccountCount) }) }}
							</span>
							<span>{{ i18n.tsx._mkgoIpRelated.elapsedDays({ n: Math.round(p.elapsedDays) }) }}</span>
						</div>
					</div>
				</div>
			</div>
			<div :class="$style.caption">{{ i18n.tsx._mkgoIpRelated.rankingBasis({ n: result.halfLifeDays }) }}</div>
			<!-- 上限で頭打ちになった候補があるときだけ。無いときに出すと存在しない表示を指す。 -->
			<div v-if="accountCountCap > 0" :class="$style.caption">
				{{ i18n.tsx._mkgoIpRelated.accountCountCapNote({ n: number(accountCountCap) }) }}
			</div>
			<div v-if="droppedTotal > 0" :class="$style.caption">
				{{ i18n.tsx._mkgoIpRelated.droppedNote({ n: number(droppedTotal) }) }}
			</div>
			<div :class="$style.caption">{{ i18n.ts._mkgoIpRelated.pagingNote }}</div>
		</div>

		<MkInfo v-if="error && errorWhilePaging" warn>{{ error }}</MkInfo>
		<MkButton v-if="result.hasMore" :disabled="loadingMore" @click="loadMore()">{{ i18n.ts.loadMore }}</MkButton>
	</template>
</div>
</template>

<script lang="ts" setup>
import { computed, ref } from 'vue';
import type * as Misskey from 'misskey-js';
import MkButton from '@/components/MkButton.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkUserCardMini from '@/components/MkUserCardMini.vue';
import { useMkSelect } from '@/composables/use-mkselect.js';
import { i18n } from '@/i18n.js';
import number from '@/filters/number.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { ipSearchErrorKind, ipSearchNotice, ipSearchOutcome } from '@/utility/ip-search-result.js';

const props = defineProps<{ userId: string }>();

// **`userId` は watch していない。** `/admin/user/:userId` は RouterView が
// full path で key するので、別の利用者へ移ると component ごと作り直される。
// その前提が変わったら、ここで検索結果を捨てる必要がある。

type SharedIP = {
	ip: string;
	targetLastSeenAt: string;
	candidateLastSeenAt: string;
	ipAccountCount: number;
	// **正確な数とは限らない。** 上限まで見えたときは「これ以上」を意味する。
	ipAccountCountIsLowerBound: boolean;
	// 減衰に使った経過日数。重みそのものは [0,1] でパーセントと見分けが付かない。
	elapsedDays: number;
};

type RelatedCandidate = {
	user: Misskey.entities.UserLite;
	isSuspended: boolean;
	isDeleted: boolean;
	lastActiveDate: string | null;
	sharedIpCount: number;
	score: number;
	sharedIps: SharedIP[];
};

type RelatedResponse = {
	user: Misskey.entities.UserLite;
	loggingEnabled: boolean;
	hasAnyHistory: boolean;
	sinceDays: number;
	retentionDays: number;
	halfLifeDays: number;
	targetIpCount: number;
	truncated: boolean;
	// **原因ごとに分かれている。両方立つことがある。** 起点を切ったなら期間を
	// 絞れば絞り込めるが、候補側の打ち切りはどうにもならない。
	targetIpsTruncated: boolean;
	candidatesTruncated: boolean;
	limit: number;
	offset: number;
	hasMore: boolean;
	droppedCount: number;
	candidates: RelatedCandidate[];
};

// mk-go 独自の endpoint なので misskey-js の型集合には無い。
function api<T>(endpoint: string, params: Record<string, unknown> = {}): Promise<T> {
	return misskeyApi(endpoint as never, params as never) as unknown as Promise<T>;
}

const loading = ref(false);
const loadingMore = ref(false);
const error = ref<string | null>(null);
const errorWhilePaging = ref(false);
const result = ref<RelatedResponse | null>(null);
const candidates = ref<RelatedCandidate[]>([]);
// **累積で持つ。** 一覧は全ページの累積なので、最新ページの値で出すと、
// 落ちの無いページを引いた時点で「落としたものは無い」という新しい嘘になる。
const droppedTotal = ref(0);
const everHadHistory = ref(false);
// 保持期間はサーバーが教える。来るまでは出さない (決め打ちを事実として描かない)。
const retentionDays = ref<number | null>(null);
// ページングで使う「実際に検索した条件」。セレクトの現在値とは別に持つ。
const searchedSinceDays = ref(90);

const {
	model: sinceDays,
	def: periodDef,
} = useMkSelect({
	items: [
		{ label: i18n.tsx._mkgoIpSearch.periodDays({ n: 7 }), value: 7 },
		{ label: i18n.tsx._mkgoIpSearch.periodDays({ n: 30 }), value: 30 },
		{ label: i18n.tsx._mkgoIpSearch.periodDays({ n: 90 }), value: 90 },
		{ label: i18n.tsx._mkgoIpSearch.periodDays({ n: 365 }), value: 365 },
	],
	initialValue: 90,
});

// 「N 件以上」と出る境目。**下限が立った候補が無ければ 0** で、そのときは
// 但し書きを出さない (存在しない表示を指してしまう)。サーバーは常に同じ上限で
// 頭打ちにするので実際は 0 か上限値のどちらかにしかならない。
const accountCountCap = computed(() => {
	let cap = 0;
	for (const c of candidates.value) {
		for (const p of c.sharedIps) {
			if (p.ipAccountCountIsLowerBound && (cap === 0 || p.ipAccountCount < cap)) cap = p.ipAccountCount;
		}
	}
	return cap;
});

const snapshot = computed(() => (result.value == null ? null : {
	loggingEnabled: result.value.loggingEnabled,
	hasAnyHistory: result.value.hasAnyHistory,
	sinceDays: result.value.sinceDays,
	retentionDays: result.value.retentionDays,
	hasMore: result.value.hasMore,
	// **起点と打ち切りを渡す。** `hasAnyHistory` はテーブル全体を見る値なので、
	// これが無いと「対象の IP 記録が 1 件も無いので照合していない」と
	// 「照合したが一致が無い」を区別できない (#3105)。
	targetIPCount: result.value.targetIpCount,
	truncated: result.value.truncated,
}));

const totals = computed(() => ({
	accountCount: candidates.value.length,
	droppedCount: droppedTotal.value,
	hasAnyHistory: everHadHistory.value,
}));

// **判定は `@/utility/ip-search-result.js` と共有する。** 「候補なし」と
// 「記録が無い」と「候補が全員消えている」の言い分けは、IP 起点 (#3104) でも
// 利用者起点 (#3105) でも同じ構造で、2 箇所に書くと片方だけ間違える。
const notice = computed(() => (snapshot.value == null ? null : ipSearchNotice(snapshot.value, totals.value)));
const outcome = computed(() => (snapshot.value == null ? 'accounts' : ipSearchOutcome(snapshot.value, totals.value)));

const status = computed(() => {
	if (loading.value) return i18n.ts._mkgoIpSearch.searching;
	if (error.value != null) return error.value;
	if (result.value == null) return '';
	if (outcome.value === 'noTargetRecords') return i18n.tsx._mkgoIpRelated.noTargetRecords({ n: result.value.sinceDays });
	if (outcome.value === 'noneOnThisPage') return i18n.ts._mkgoIpSearch.noneOnThisPage;
	if (outcome.value === 'noneResolvable') return i18n.ts._mkgoIpRelated.noneResolvable;
	if (outcome.value === 'noneResolvablePartial') return i18n.ts._mkgoIpRelated.noneResolvablePartial;
	if (outcome.value === 'partial') return i18n.ts._mkgoIpRelated.noneInSearchedRange;
	if (outcome.value === 'noMatch') return i18n.ts._mkgoIpRelated.noMatch;
	if (outcome.value === 'noMatchInPeriod') return i18n.ts._mkgoIpRelated.noMatchInPeriod;
	if (candidates.value.length === 0) return i18n.ts._mkgoIpSearch.noneFound;
	return i18n.tsx._mkgoIpSearch.foundAccounts({ n: number(candidates.value.length) });
});

/**
 * Display position in the accumulated list.
 *
 * **サーバーの順位そのものではない。** 利用者の行を解決できなかった候補を
 * 落としているので、前のページで落ちた分だけ番号が詰まる。単調ではあるので
 * 「上ほど手掛かりが強い」は読めるが、「N 位」とは読ませない。
 */
function displayOrder(index: number): number {
	return index + 1;
}

/**
 * **世代で古い応答を捨てる。** 「さらに表示」の最中も期間を変えて引き直せるので、
 * 捨てないと別の条件の結果が同じ一覧へ継ぎ足される。
 */
let generation = 0;

async function search(offset: number) {
	const first = offset === 0;
	const days = first ? sinceDays.value : searchedSinceDays.value;
	error.value = null;
	errorWhilePaging.value = false;
	const gen = ++generation;
	if (first) {
		loading.value = true;
		candidates.value = [];
		droppedTotal.value = 0;
		everHadHistory.value = false;
		searchedSinceDays.value = days;
	} else {
		loadingMore.value = true;
	}
	try {
		const res = await api<RelatedResponse>('admin/ip/related-accounts', {
			userId: props.userId,
			sinceDays: days,
			offset,
		});
		if (gen !== generation) return;
		result.value = res;
		retentionDays.value = res.retentionDays;
		candidates.value = first ? res.candidates : mergeCandidates(candidates.value, res.candidates);
		droppedTotal.value += res.droppedCount;
		everHadHistory.value = everHadHistory.value || res.hasAnyHistory;
	} catch (err) {
		if (gen !== generation) return;
		// **前回の結果を消す。** 残したまま失敗だけ添えると、古い候補を
		// 今回の結果として読ませることになる。
		if (first) {
			result.value = null;
			candidates.value = [];
			droppedTotal.value = 0;
			everHadHistory.value = false;
		}
		error.value = errorMessage(err, first);
		errorWhilePaging.value = !first;
	} finally {
		// 最新の要求だけが解除する。
		if (gen === generation) {
			loading.value = false;
			loadingMore.value = false;
		}
	}
}

/**
 * **この画面に IP の入力欄は無い。** 共有の分類は初回の `INVALID_PARAM` を
 * 「IP アドレスとして読めません」に写すが、ここで初回に 400 が返る現実的な
 * 原因は**対象がリモート利用者**であること (期間は固定の選択肢から選ぶ)。
 * 入力欄の無い画面で入力を直せと言わない。
 */
function errorMessage(err: unknown, first: boolean): string {
	const kind = ipSearchErrorKind(err, first);
	if (kind === 'notAnIp') return i18n.ts._mkgoIpRelated.notLocalUser;
	return i18n.ts._mkgoIpSearch[kind];
}

// **追記のときは userId で重複を落とす。** offset ページングなので、ページを
// 送る間に観測が入ると順位が動き、直前のページの末尾が次の先頭に再登場しうる。
function mergeCandidates(current: RelatedCandidate[], incoming: RelatedCandidate[]): RelatedCandidate[] {
	const seen = new Set(current.map(c => c.user.id));
	return [...current, ...incoming.filter(c => !seen.has(c.user.id))];
}

/** 次の offset は `offset + limit`。表示件数ではない (落とした分を読み飛ばす)。 */
function loadMore() {
	if (result.value == null) return Promise.resolve();
	return search(result.value.offset + result.value.limit);
}
</script>

<style lang="scss" module>
.placeholder {
	padding: 32px;
	text-align: center;
}

.row {
	padding: 12px 0;
}

.row + .row {
	border-top: solid 0.5px var(--MI_THEME-divider);
}

.rank {
	font-size: 0.85em;
	opacity: 0.7;
	margin-bottom: 4px;
}

.card {
	display: block;
}

.meta {
	display: flex;
	gap: 6px;
	flex-wrap: wrap;
	margin-top: 8px;

	&:empty {
		margin-top: 0;
	}
}

.badge {
	padding: 2px 8px;
	border-radius: 999px;
	font-size: 0.85em;
}

.danger {
	color: var(--MI_THEME-error);
	border: solid 1px var(--MI_THEME-error);
}

.evidence {
	margin-top: 8px;
	display: grid;
	gap: 8px;
}

.match {
	padding: 8px;
	border-radius: var(--MI-radius-sm);
	border: solid 0.5px var(--MI_THEME-divider);
}

.matchFacts {
	margin-top: 4px;
	display: grid;
	gap: 2px;
	font-size: 0.85em;
	opacity: 0.8;
}

.caption {
	font-size: 0.85em;
	opacity: 0.7;
}

/* 読み上げ専用。display:none にすると読まれない。 */
.status {
	position: absolute;
	width: 1px;
	height: 1px;
	margin: -1px;
	padding: 0;
	overflow: hidden;
	clip-path: inset(50%);
	white-space: nowrap;
}
</style>
