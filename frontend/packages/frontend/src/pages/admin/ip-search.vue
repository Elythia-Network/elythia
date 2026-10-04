<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: IP アドレスからローカルアカウントを探す画面 (#3104 / 親 #3066)。

	純正には逆向き (利用者 → IP、`admin/get-user-ips`) しか無い。荒らしの
	使い捨てアカウントを追うときに要るのはこちら向きで、純正では DB を直接
	引くしかなかった。

	**出るのは候補であって判定ではない。** 同じ IP を使ったことは同一人物である
	ことを意味しない (家庭・職場・学校・公衆 Wi-Fi・携帯回線の CGNAT・VPN)。
	注意書きは結果の上に常設し、結果が出たときだけ出す形にしない。
-->
<template>
<PageWithHeader>
	<div class="_spacer" style="--MI_SPACER-w: 800px; --MI_SPACER-min: 16px; --MI_SPACER-max: 32px;">
		<div class="_gaps_m">
			<MkInfo>{{ i18n.ts._mkgoIpSearch.disclaimer }}</MkInfo>

			<!--
				**live region は常設する。** 中身と一緒に v-if で挿入すると、
				初回の結果は読み上げられない。ここには短い状況だけを入れる
				(一覧そのものを region にすると 30 件ぶんが読み上げられる)。
			-->
			<div :class="$style.status" aria-live="polite">{{ status }}</div>

			<form class="_gaps_s" @submit.prevent="search(0)">
				<MkInput v-model="ip" type="search" :spellcheck="false" :placeholder="i18n.ts._mkgoIpSearch.ipPlaceholder">
					<template #label>{{ i18n.ts._mkgoIpSearch.ipAddress }}</template>
				</MkInput>
				<MkSelect v-model="sinceDays" :items="periodDef">
					<template #label>{{ i18n.ts._mkgoIpSearch.period }}</template>
					<!--
						**何を絞る期間なのかを言う。** SQL の条件は最終観測なので、
						「直近 7 日」の結果に「最初の観測: 100 日前」が並ぶ。
						保持期間はサーバーが教えるので、来るまでは出さない
						(決め打ちを事実として描かない)。
					-->
					<template #caption>
						{{ i18n.ts._mkgoIpSearch.periodMeaning }}
						<template v-if="retentionDays != null">{{ i18n.tsx._mkgoIpSearch.retentionNote({ n: retentionDays }) }}</template>
					</template>
				</MkSelect>
				<MkButton primary type="submit" :disabled="loading || ip.trim() === ''"><i class="ti ti-search"></i> {{ i18n.ts.search }}</MkButton>
			</form>

			<!--
				**ページング由来の失敗はここに出さない。** 「さらに表示」は一覧の
				いちばん下にあるので、数ページ読んだ後の失敗を画面の最上部へ出すと
				視界の外で消える。押したボタンの隣に出す (下の同じ MkInfo)。
			-->
			<MkInfo v-if="error && !errorWhilePaging" warn>{{ error }}</MkInfo>

			<div v-if="loading" :class="$style.placeholder"><MkLoading/></div>

			<template v-else-if="result">
				<!--
					**「記録が無効」と「結果がある」は同時に成り立つ。** 無効にした
					後も残っている記録は引けるので、単一の状態に潰さない (#3066 §7)。
				-->
				<MkInfo v-if="notice === 'loggingDisabled'" warn>{{ i18n.ts._mkgoIpSearch.loggingDisabled }}</MkInfo>
				<MkInfo v-else-if="notice === 'loggingDisabledNoHistory'" warn>{{ i18n.ts._mkgoIpSearch.loggingDisabledNoHistory }}</MkInfo>
				<MkInfo v-else-if="notice === 'noHistory'" warn>{{ i18n.tsx._mkgoIpSearch.noHistory({ n: result.retentionDays }) }}</MkInfo>

				<!--
					**検索した条件は IP と期間の両方を出す。** 期間のセレクトは検索の
					後も自由に動かせるので、IP だけ出すと「セレクトは 365 日、本文は
					『この期間に記録がありません』」という、調べた範囲を偽る並びになる。
				-->
				<MkKeyValue oneline>
					<template #key>{{ i18n.ts._mkgoIpSearch.searched }}</template>
					<template #value><span class="_monospace">{{ result.ip }}</span></template>
				</MkKeyValue>
				<MkKeyValue oneline>
					<template #key>{{ i18n.ts._mkgoIpSearch.period }}</template>
					<template #value>{{ i18n.tsx._mkgoIpSearch.periodDays({ n: result.sinceDays }) }}</template>
				</MkKeyValue>

				<!--
					**判定は `@/utility/ip-search-result.js` が持つ。** 「該当なし」と
					「記録が無い」と「候補が全員消えている」の言い分けをここで 2 度
					間違えた (`hasMore` で分けて最後のページに嘘が残り、ページごとの
					`droppedCount` を見て累積の一覧と食い違った) ので、分岐を純粋関数
					へ出して `test/unit/ip-search-result.test.ts` で表にして固定した。
				-->
				<MkInfo v-if="outcome === 'noneOnThisPage'">{{ i18n.ts._mkgoIpSearch.noneOnThisPage }}</MkInfo>
				<MkInfo v-else-if="outcome === 'noneResolvable'">{{ i18n.ts._mkgoIpSearch.noneResolvable }}</MkInfo>
				<MkInfo v-else-if="outcome === 'noMatch'">{{ i18n.ts._mkgoIpSearch.noMatch }}</MkInfo>
				<MkInfo v-else-if="outcome === 'noMatchInPeriod'">{{ i18n.ts._mkgoIpSearch.noMatchInPeriod }}</MkInfo>

				<div v-if="accounts.length > 0" class="_gaps_s">
					<div v-for="a in accounts" :key="a.user.id" :class="$style.row">
						<MkA :to="`/admin/user/${a.user.id}`" :class="$style.card">
							<MkUserCardMini :user="a.user" :withChart="false"/>
						</MkA>
						<div :class="$style.meta">
							<span v-if="a.isSuspended" :class="[$style.badge, $style.danger]">{{ i18n.ts._mkgoIpSearch.suspended }}</span>
							<span v-if="a.isDeleted" :class="[$style.badge, $style.danger]">{{ i18n.ts._mkgoIpSearch.deleted }}</span>
						</div>
						<div :class="$style.facts">
							<MkKeyValue oneline>
								<template #key>{{ i18n.ts._mkgoIpSearch.firstSeen }}</template>
								<template #value><MkTime :time="a.firstSeenAt" mode="detail"/></template>
							</MkKeyValue>
							<MkKeyValue oneline>
								<template #key>{{ i18n.ts._mkgoIpSearch.lastSeen }}</template>
								<template #value><MkTime :time="a.lastSeenAt" mode="detail"/></template>
							</MkKeyValue>
							<MkKeyValue oneline>
								<template #key>{{ i18n.ts._mkgoIpSearch.observationCount }}</template>
								<template #value>{{ i18n.tsx._mkgoIpSearch.observationCountValue({ n: number(a.observationCount) }) }}</template>
							</MkKeyValue>
							<MkKeyValue oneline>
								<template #key>{{ i18n.ts.lastActiveDate }}</template>
								<template #value>
									<MkTime v-if="a.lastActiveDate != null" :time="a.lastActiveDate" mode="detail"/>
									<span v-else>{{ i18n.ts._mkgoIpSearch.lastActiveUnknown }}</span>
								</template>
							</MkKeyValue>
						</div>
					</div>
					<!--
						**落とした件数は候補が出ているときも伝える。** 黙って減らすと
						「これで全部」と読まれる。消えたアカウントも調査の材料になる。
					-->
					<div v-if="droppedTotal > 0" :class="$style.caption">
						{{ i18n.tsx._mkgoIpSearch.droppedNote({ n: number(droppedTotal) }) }}
					</div>
					<div :class="$style.caption">{{ i18n.ts._mkgoIpSearch.observationCaption }}</div>
				</div>

				<!--
					**結果ブロックの外に置く。** 1 ページが丸ごと「利用者の行を引けない
					観測」だと accounts は空になるが、次のページにはまだ候補が居る。
					中に置くと、そこから先へ進めなくなる。
				-->
				<MkInfo v-if="error && errorWhilePaging" warn>{{ error }}</MkInfo>
				<MkButton v-if="result.hasMore" :disabled="loadingMore" @click="loadMore()">{{ i18n.ts.loadMore }}</MkButton>
			</template>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { computed, reactive, ref } from 'vue';
import type * as Misskey from 'misskey-js';
import MkButton from '@/components/MkButton.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkInput from '@/components/MkInput.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkUserCardMini from '@/components/MkUserCardMini.vue';
import { useMkSelect } from '@/composables/use-mkselect.js';
import { i18n } from '@/i18n.js';
import number from '@/filters/number.js';
import { definePage } from '@/page.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { ipSearchErrorKind, ipSearchNotice, ipSearchOutcome } from '@/utility/ip-search-result.js';

type IPAccount = {
	user: Misskey.entities.UserLite;
	isSuspended: boolean;
	isDeleted: boolean;
	lastActiveDate: string | null;
	firstSeenAt: string;
	lastSeenAt: string;
	observationCount: number;
};

type IPAccountsResponse = {
	ip: string;
	loggingEnabled: boolean;
	hasAnyHistory: boolean;
	sinceDays: number;
	retentionDays: number;
	limit: number;
	offset: number;
	hasMore: boolean;
	droppedCount: number;
	accounts: IPAccount[];
};

// mk-go 独自のエンドポイントなので misskey-js の型集合には無い。
// signup-applications.vue と同じ理由の cast。
function api<T>(endpoint: string, params: Record<string, unknown> = {}): Promise<T> {
	return misskeyApi(endpoint as never, params as never) as unknown as Promise<T>;
}

// 保持期間はサーバーが教える。**画面で決め打ちしない** — 決め打ちした値を
// 事実として出すと、サーバーが変えたときに黙って嘘になる。来るまでは出さない。
const retentionDays = ref<number | null>(null);

const ip = ref('');
const loading = ref(false);
const loadingMore = ref(false);
const error = ref<string | null>(null);
// 失敗がページング由来かどうか。一覧の末尾で押したボタンの失敗を画面の最上部に
// 出すと、数ページ読んだ後では視界の外に出る。
const errorWhilePaging = ref(false);
const result = ref<IPAccountsResponse | null>(null);
const accounts = ref<IPAccount[]>([]);
// **累積で持つ。** 一覧は全ページの累積なので、注釈や判定を最新ページの値で
// 出すと、落ちの無いページを引いた時点で「落としたものは無い」という新しい嘘に
// なる。`hasAnyHistory` も同じ理由で「どれか 1 ページでも真だったか」を持つ。
const droppedTotal = ref(0);
const everHadHistory = ref(false);
// ページングで使う「実際に検索した条件」。入力欄とは別に持つ。
const searched = reactive({ ip: '', sinceDays: 90 });

const {
	model: sinceDays,
	def: periodDef,
} = useMkSelect({
	// **リテラルで書く。** `map` で作ると value が number に潰れ、
	// `useMkSelect` の initialValue の型検査 (全 item を含むか) が通らない。
	items: [
		{ label: i18n.tsx._mkgoIpSearch.periodDays({ n: 7 }), value: 7 },
		{ label: i18n.tsx._mkgoIpSearch.periodDays({ n: 30 }), value: 30 },
		{ label: i18n.tsx._mkgoIpSearch.periodDays({ n: 90 }), value: 90 },
		{ label: i18n.tsx._mkgoIpSearch.periodDays({ n: 365 }), value: 365 },
	],
	initialValue: 90,
});

/**
 * Runs one page of the search.
 *
 * **2 ページ目以降は最初の検索の条件を使う。** 入力欄の現在値を読むと、
 * 利用者が IP や期間を変えた後に「さらに表示」を押したときに**別の検索の結果を
 * 同じ一覧へ継ぎ足す**ことになる。
 */
/**
 * **世代で古い応答を捨てる。** 「さらに表示」の最中は `loading` が false なので
 * フォームは生きており、そこで別の IP を検索できる。捨てないと (a) 応答の順番に
 * よっては「検索したアドレス」と一覧の中身が食い違い、(b) 次の「さらに表示」が
 * 別の結果集合から採った offset を渡して**以降の候補が永久に出てこない**。
 * `admin-user.emoji-applications.vue` が同じ形で解いている。
 */
let generation = 0;

async function search(offset: number) {
	const first = offset === 0;
	const raw = first ? ip.value.trim() : searched.ip;
	const days = first ? sinceDays.value : searched.sinceDays;
	// **失敗の表示は入力の検査より先に消す。** 後ろに置くと、空のまま押したときに
	// 前回のエラーが残り、押したこと自体が伝わらない。
	error.value = null;
	errorWhilePaging.value = false;
	if (raw === '') return;
	// **形の判定はサーバーに任せる。** ここでは空かどうかだけ見る
	// (記録側と同じ正規化を 2 箇所に持たない)。
	const gen = ++generation;
	if (first) {
		loading.value = true;
		accounts.value = [];
		droppedTotal.value = 0;
		everHadHistory.value = false;
		searched.ip = raw;
		searched.sinceDays = days;
	} else {
		loadingMore.value = true;
	}
	try {
		const res = await api<IPAccountsResponse>('admin/ip/accounts', {
			ip: raw,
			sinceDays: days,
			offset,
		});
		if (gen !== generation) return;
		// **案内文は最新ページの result、一覧は累積の accounts を読む。**
		// ページ送りの途中で `user_ip` が空になる (掃除が走る) と、page2 の
		// `hasAnyHistory: false` が page1 由来の一覧の上に出る。窓は極めて狭い
		// ので放置しているが、2 つのスナップショットを同じ画面に混ぜている。
		result.value = res;
		retentionDays.value = res.retentionDays;
		// **追記のときは userId で重複を落とす。** offset ページングなので、
		// ページを送る間に観測が入ると行が後ろへずれ、直前のページの末尾が
		// 次のページの先頭に再登場しうる (Vue の duplicate key にもなる)。
		accounts.value = first ? res.accounts : mergeAccounts(accounts.value, res.accounts);
		droppedTotal.value += res.droppedCount;
		everHadHistory.value = everHadHistory.value || res.hasAnyHistory;
	} catch (err) {
		if (gen !== generation) return;
		// **前回の結果を消す。** 残したまま失敗だけ添えると、古い候補を
		// 今回の検索結果として読ませることになる。
		if (first) {
			result.value = null;
			accounts.value = [];
			droppedTotal.value = 0;
			everHadHistory.value = false;
		}
		// **`INVALID_PARAM` を「IP が読めない」に写すのは初回だけ。** ページングで
		// offset の上限に当たっても同じコードが返るので、入力欄と無関係な指摘になる。
		error.value = errorMessage(err, first);
		errorWhilePaging.value = !first;
	} finally {
		// **最新の要求だけが解除する。** 古い応答が解除すると、まだ飛んでいるのに
		// スピナーが消えて古い結果が「今の結果」として描かれる。
		if (gen === generation) {
			loading.value = false;
			loadingMore.value = false;
		}
	}
}

function mergeAccounts(current: IPAccount[], incoming: IPAccount[]): IPAccount[] {
	const seen = new Set(current.map(a => a.user.id));
	return [...current, ...incoming.filter(a => !seen.has(a.user.id))];
}

/** 分類は `@/utility/ip-search-result.js`。ここは文面を当てるだけ。 */
function errorMessage(err: unknown, first: boolean): string {
	return i18n.ts._mkgoIpSearch[ipSearchErrorKind(err, first)];
}

/**
 * **次の offset は消費した行数で、表示件数ではない。**
 * サーバーは `limit` 行引いてから「利用者の行を引けない観測」を落とすので、
 * `accounts.length` を offset にすると落とした分だけ候補を読み飛ばす。
 */
const snapshot = computed(() => (result.value == null ? null : {
	loggingEnabled: result.value.loggingEnabled,
	hasAnyHistory: result.value.hasAnyHistory,
	sinceDays: result.value.sinceDays,
	retentionDays: result.value.retentionDays,
	hasMore: result.value.hasMore,
}));

const totals = computed(() => ({
	accountCount: accounts.value.length,
	droppedCount: droppedTotal.value,
	hasAnyHistory: everHadHistory.value,
}));

const notice = computed(() => (snapshot.value == null ? null : ipSearchNotice(snapshot.value, totals.value)));
const outcome = computed(() => (snapshot.value == null ? 'accounts' : ipSearchOutcome(snapshot.value, totals.value)));

/**
 * Short sentence for the live region.
 *
 * **一覧そのものを読み上げさせない。** 30 件のカードが丸ごと読まれるので、
 * 件数と結末だけを入れる。
 */
const status = computed(() => {
	if (loading.value) return i18n.ts._mkgoIpSearch.searching;
	if (error.value != null) return error.value;
	if (result.value == null) return '';
	// **画面と同じことを言う。** 「候補は見つかりませんでした」だけを読み上げると、
	// 「記録はあるが候補が全員消えている」という**この機能でいちばん重要な区別**が
	// 視覚表示にしか無い状態になる。
	//
	// **enum を直接 index しない。** 共有モジュールに結末が増えると、この画面に
	// 無いキーを引いて `undefined` を読み上げる (実際 #3105 で増えた)。
	switch (outcome.value) {
		case 'noneOnThisPage': return i18n.ts._mkgoIpSearch.noneOnThisPage;
		case 'noneResolvable': return i18n.ts._mkgoIpSearch.noneResolvable;
		case 'noMatch': return i18n.ts._mkgoIpSearch.noMatch;
		case 'noMatchInPeriod': return i18n.ts._mkgoIpSearch.noMatchInPeriod;
		// `noTargetRecords` / `partial` は利用者を起点にする検索でだけ出る
		// (この画面は `targetIPCount` も `truncated` も渡さない)。
		default: break;
	}
	if (accounts.value.length === 0) return i18n.ts._mkgoIpSearch.noneFound;
	return i18n.tsx._mkgoIpSearch.foundAccounts({ n: number(accounts.value.length) });
});

function loadMore() {
	if (result.value == null) return Promise.resolve();
	return search(result.value.offset + result.value.limit);
}

definePage(() => ({
	title: i18n.ts._mkgoIpSearch.title,
	icon: 'ti ti-network',
}));
</script>

<style lang="scss" module>
.placeholder {
	padding: 32px;
	text-align: center;
}

/*
	**背景を敷かない。** 中に置く MkUserCardMini 自身が `--MI_THEME-panel` を
	持つので、同じ色を重ねるとカードが枠に溶けて 1 件の区切りが見えなくなる。

	**区切りは `+` で入れる。** `:last-child` は当たらない — この div の最後の子は
	注釈のほうなので、末尾の行を指せない。
*/
.row {
	padding: 12px 0;
}

.row + .row {
	border-top: solid 0.5px var(--MI_THEME-divider);
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
