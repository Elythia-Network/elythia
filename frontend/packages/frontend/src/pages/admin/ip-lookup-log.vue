<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: IP 照会の監査記録を読む画面 (#3106 / 親 #3066 / #3276)。

	IP とアカウントの対応は機密性の高いモデレーション情報なので、**照会そのものを
	記録する**。ここはそれを読む側。

	**この画面自体が機密。** 照会に使った IP がそのまま並ぶので、照会と同じ 3 段の
	権限 (moderator + canSearchIpHistory + read:admin:user-ips) で守られている。
	コンポーネントは権限を確認したモデレーションログのタブからだけ mount するが、
	権限と API scope の最終判定はサーバー側で行う。

	**記録されるのは照会の事実だけで、結果は残っていない。** 「何件返したか」は
	あるが「誰が候補に出たか」は無い — 記録すると、この表が第 2 の「IP とアカウント
	の対応」になるため。
-->
<template>
<div class="_gaps_m">
	<MkInfo>{{ i18n.ts._mkgoIpLookupLog.about }}</MkInfo>

	<div :class="$style.status" aria-live="polite">{{ status }}</div>
	<MkInfo v-if="error && !errorWhilePaging" warn>{{ error }}</MkInfo>
	<div v-if="loading" :class="$style.placeholder"><MkLoading/></div>

	<template v-else-if="result">
		<!--
			**保持期間はサーバーが教える。** 画面で決め打ちすると、サーバーが
			変えたときに黙って嘘になる。
		-->
		<MkInfo>{{ i18n.tsx._mkgoIpLookupLog.retentionNote({ n: result.retentionDays }) }}</MkInfo>
		<!--
			**「照会されていない」とは書かない。** 記録が空でも、保持期間を
			過ぎて消えたのか一度も引かれていないのかは、この応答からは
			区別できない (#2792 と同じ形の言い過ぎを避ける)。
		-->
		<MkInfo v-if="entries.length === 0">{{ i18n.tsx._mkgoIpLookupLog.empty({ n: result.retentionDays }) }}</MkInfo>

		<div v-else class="_gaps_s">
			<MkTl :events="timeline" groupBy="d">
				<template #left="{ event }">
					<MkAvatar v-if="event.user" :user="event.user" style="width: 26px; height: 26px;"/>
					<i v-else class="ti ti-user-off"></i>
				</template>
				<template #right="{ event: e }">
					<MkFolder :key="e.id">
						<template #label>{{ kindLabel(e.kind) }}</template>
						<template #caption><MkTime :time="e.createdAt" mode="detail"/></template>

						<!--
							**照会した人を引けないことがある。** `ip_lookup_log.userId`
							に FK は無いので、退会しても記録は残る (監査の目的からして
							残すのが正しい)。引けないときは id だけ出す。
						-->
						<MkA v-if="e.user" :to="`/admin/user/${e.user.id}`" :class="$style.card">
							<MkUserCardMini :user="e.user" :withChart="false"/>
						</MkA>
						<div v-else :class="$style.goneUser">
							{{ i18n.ts._mkgoIpLookupLog.userGone }}
							<span class="_monospace">{{ e.userId }}</span>
						</div>

						<div :class="$style.facts">
							<MkKeyValue oneline>
								<template #key>{{ i18n.ts._mkgoIpLookupLog.at }}</template>
								<template #value><MkTime :time="e.createdAt" mode="detail"/></template>
							</MkKeyValue>
							<MkKeyValue oneline>
								<template #key>{{ i18n.ts._mkgoIpLookupLog.kind }}</template>
								<template #value>{{ kindLabel(e.kind) }}</template>
							</MkKeyValue>
							<!--
								**起点は種類で出し分ける。** IP 起点なら IP、利用者
								起点なら対象の利用者。両方の欄を常に出すと、空のほうを
								「記録が欠けている」と読ませる。
							-->
							<MkKeyValue oneline>
								<template #key>{{ i18n.ts._mkgoIpLookupLog.subject }}</template>
								<template #value>
									<span v-if="e.kind === 'ip'" class="_monospace">{{ e.ip }}</span>
									<MkA v-else-if="e.targetUser" :to="`/admin/user/${e.targetUser.id}`">@{{ e.targetUser.username }}</MkA>
									<span v-else class="_monospace">{{ e.targetUserId }}</span>
								</template>
							</MkKeyValue>
							<!--
								**期間を取らない照会がある。** upstream の
								`admin/get-user-ips` は窓ではなく最新 30 件を返すので
								`sinceDays` が 0 になる。そのまま出すと「直近 0 日」という
								存在しない条件を表示する。
							-->
							<MkKeyValue oneline>
								<template #key>{{ i18n.ts._mkgoIpLookupLog.period }}</template>
								<template #value>
									<template v-if="e.sinceDays > 0">{{ i18n.tsx._mkgoIpLookupLog.periodDays({ n: e.sinceDays }) }}</template>
									<template v-else>{{ i18n.ts._mkgoIpLookupLog.noPeriod }}</template>
								</template>
							</MkKeyValue>
							<MkKeyValue oneline>
								<template #key>{{ i18n.ts._mkgoIpLookupLog.resultCount }}</template>
								<template #value>{{ i18n.tsx._mkgoIpLookupLog.resultCountValue({ n: number(e.resultCount) }) }}</template>
							</MkKeyValue>
						</div>
					</MkFolder>
				</template>
			</MkTl>
			<div :class="$style.caption">{{ i18n.ts._mkgoIpLookupLog.resultsNotRecorded }}</div>
		</div>

		<MkInfo v-if="error && errorWhilePaging" warn>{{ error }}</MkInfo>
		<MkButton v-if="result.hasMore" :disabled="loadingMore" @click="loadMore()">{{ i18n.ts.loadMore }}</MkButton>
	</template>
</div>
</template>

<script lang="ts" setup>
import { computed, onMounted, ref } from 'vue';
import type * as Elythia from 'elythia-js';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkTl from '@/components/MkTl.vue';
import MkUserCardMini from '@/components/MkUserCardMini.vue';
import { i18n } from '@/i18n.js';
import number from '@/filters/number.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { ipSearchErrorKind } from '@/utility/ip-search-result.js';

type IPLookupLogEntry = Elythia.IPLookupLogEntry;
type IPLookupLogResponse = Elythia.IPLookupLogResult;

const loading = ref(true);
const loadingMore = ref(false);
const error = ref<string | null>(null);
// 失敗がページング由来かどうか。「さらに表示」は一覧の末尾にあるので、そこでの
// 失敗を画面の最上部に出すと視界の外で消える (ip-search.vue と同じ理由)。
const errorWhilePaging = ref(false);
const result = ref<IPLookupLogResponse | null>(null);
const entries = ref<IPLookupLogEntry[]>([]);
const timeline = computed(() => entries.value.map(e => ({
	id: e.id,
	timestamp: new Date(e.createdAt).getTime(),
	data: e,
})));

// **世代で古い応答を捨てる。** 「さらに表示」の最中でも再読み込みは掛けられるので、
// 捨てないと別の取得の offset を継ぎ足して以降の記録が出てこなくなる。
let generation = 0;

async function load(offset: number) {
	const first = offset === 0;
	error.value = null;
	errorWhilePaging.value = false;
	const gen = ++generation;
	if (first) {
		loading.value = true;
		entries.value = [];
	} else {
		loadingMore.value = true;
	}
	try {
		const res = await misskeyApi('admin/ip/lookup-log', { offset });
		if (gen !== generation) return;
		result.value = res;
		// **追記のときは id で重複を落とす。** offset ページングなので、ページを
		// 送る間に新しい照会が入ると行が後ろへずれ、直前のページの末尾が次の
		// ページの先頭に再登場しうる (Vue の duplicate key にもなる)。
		entries.value = first ? res.entries : mergeEntries(entries.value, res.entries);
	} catch (err) {
		if (gen !== generation) return;
		// **前回の結果を消す。** 残したまま失敗だけ添えると、古い一覧を今回の
		// 取得結果として読ませることになる。
		if (first) {
			result.value = null;
			entries.value = [];
		}
		error.value = errorMessage(err);
		errorWhilePaging.value = !first;
	} finally {
		if (gen === generation) {
			loading.value = false;
			loadingMore.value = false;
		}
	}
}

/**
 * 分類は ip-search と共有する。**`first` は常に false** — この画面は利用者の
 * 入力を取らないので、`INVALID_PARAM` を「IP が読めない」に写すと、入力欄の
 * 無い画面で入力を直せと言うことになる。
 *
 * **`pagingLimit` の文面だけは共有しない。** 共有側は「対象期間を絞ってください」
 * と言うが、この画面に期間の指定は無い (当たるのは offset の上限だけ)。
 */
function errorMessage(err: unknown): string {
	const kind = ipSearchErrorKind(err, false);
	if (kind === 'pagingLimit') return i18n.ts._mkgoIpLookupLog.pagingLimit;
	return i18n.ts._mkgoIpSearch[kind];
}

function mergeEntries(current: IPLookupLogEntry[], incoming: IPLookupLogEntry[]): IPLookupLogEntry[] {
	const seen = new Set(current.map(e => e.id));
	return [...current, ...incoming.filter(e => !seen.has(e.id))];
}

/**
 * **enum を直接 index しない。** サーバーが種類を足したときに `undefined` を
 * 描かないよう、知らない値はそのまま出す (#3105 で同じ形を踏んだ)。
 */
function kindLabel(kind: string): string {
	switch (kind) {
		case 'ip': return i18n.ts._mkgoIpLookupLog.kindIp;
		case 'relatedAccounts': return i18n.ts._mkgoIpLookupLog.kindRelated;
		case 'userIps': return i18n.ts._mkgoIpLookupLog.kindUserIps;
		case 'signins': return i18n.ts._mkgoIpLookupLog.kindSignins;
		default: return kind;
	}
}

const status = computed(() => {
	if (loading.value) return i18n.ts._mkgoIpLookupLog.loading;
	if (error.value != null) return error.value;
	if (result.value == null) return '';
	if (entries.value.length === 0) return i18n.tsx._mkgoIpLookupLog.empty({ n: result.value.retentionDays });
	return i18n.tsx._mkgoIpLookupLog.found({ n: number(entries.value.length) });
});

function loadMore() {
	if (result.value == null) return Promise.resolve();
	return load(result.value.offset + result.value.limit);
}

onMounted(() => load(0));
</script>

<style lang="scss" module>
.placeholder {
	padding: 32px;
	text-align: center;
}

/* 読み上げ専用。見た目には出さないが display:none にすると読まれない。 */
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

.card {
	display: block;
}

.goneUser {
	font-size: 0.9em;
	opacity: 0.7;
}

.facts {
	margin-top: 8px;
	display: grid;
	gap: 4px;
}

.caption {
	font-size: 0.85em;
	opacity: 0.7;
}
</style>
