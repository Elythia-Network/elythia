<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<!--
	**取得できなかったことを黙って隠さない (#2960)。** 何も出さないと
	「過去の申請は無い」と読める。このページが nameConflict / remoteGone で
	採っているのと同じ判断 — 確認できていないことを隠すと、実際には履歴が
	ある申請を承認してしまう。
-->
<!--
	**可視になるまで取りに行かない (#2960 レビュー H1)。** 審査待ちタブは
	`defaultOpen` で**全行が最初から開いている**ので、`onMounted` で取ると
	limit 50 のときに 50 本の related が同時に飛ぶ。「閉じている行では走らない」
	は既定タブでは成り立たない。`v-appear` (IntersectionObserver) なら画面に
	入った行だけが取りに行く。
-->
<div v-if="!loaded" v-appear="onAppear" :class="$style.placeholder">
	<MkLoading v-if="fetching" :em="true"/>
</div>
<!--
	**初回の取得に失敗したときも再試行できるようにする (レビュー R2-M1)。**
	この分岐は `MkFolder` の外なので、折りたたみの中に置いた再試行ボタンは
	描画されない。`MkFolder` は一度開いた body を閉じても unmount しない
	ので、開き直しても `loaded` は true のままで、**復旧手段がページの
	リロードしか無くなる**。いちばん起きやすい「初回の通信失敗」がそれ。
-->
<div v-else-if="failed && items.length === 0" class="_gaps_s">
	<MkInfo warn>{{ i18n.ts._emojiApplication.relatedUnknown }}</MkInfo>
	<MkButton :disabled="fetching" @click="loadMore">{{ i18n.ts.retry }}</MkButton>
</div>
<MkFolder v-else-if="counts && counts.total > 0">
	<template #icon><i class="ti ti-history"></i></template>
	<!-- **開かなくても件数と内訳が見える。** 見るべき履歴かどうかを、開く前に判断できる。 -->
	<template #label>{{ summaryLabel }}</template>

	<div class="_gaps_s">
		<!--
			**過去に却下されていたことだけを理由に拒否しない。** ライセンスの変更・
			画像の修正・運用方針の変更がありうるので、これは審査を補助する情報。
		-->
		<MkInfo>{{ i18n.ts._emojiApplication.relatedNote }}</MkInfo>

		<div v-for="item in items" :key="item.id" :class="$style.row">
			<div :class="$style.thumb">
				<img v-if="previewUrls.get(item.id)" :src="previewUrls.get(item.id)!" :alt="item.name" :class="$style.thumbImg" @error="onPreviewError(item)"/>
				<!--
					**「確認できなかった」と「消された」を分ける (レビュー R2-L4)。**
					確定していないものを「ありません」と言い切ると、実際には残って
					いる申請を却下しうる。審査一覧と同じ判断。
				-->
				<span v-else :class="$style.thumbGone">{{ relatedImageMissingLabel(item, brokenPreviews) }}</span>
			</div>
			<div :class="$style.body">
				<div :class="$style.head">
					<!-- 自作画像かリモート絵文字か。本体一覧と同じアイコンで示す。 -->
					<i :class="item.remoteHost ? 'ti ti-world-download' : 'ti ti-mood-smile'"></i>
					<span class="_monospace">:{{ item.name }}:</span>
					<span :class="[$style.status, $style[item.status]]">{{ relatedStatusLabel(item.status) }}</span>
				</div>
				<div :class="$style.meta">
					<!-- どの条件で一致したか。名前を変えた再申請を見落とさないための手がかり。 -->
					<span v-for="m in item.matchedBy" :key="m" :class="$style.badge">{{ matchedByLabel(m) }}</span>
				</div>
				<MkKeyValue v-if="item.remoteHost" oneline>
					<template #key>{{ i18n.ts._emojiApplication.remoteSource }}</template>
					<template #value><span class="_monospace">:{{ item.remoteName }}:@{{ item.remoteHost }}</span></template>
				</MkKeyValue>
				<MkKeyValue oneline>
					<template #key>{{ i18n.ts._emojiApplication.applicant }}</template>
					<template #value><MkA :to="`/admin/user/${item.userId}`" class="_link">{{ item.userId }}</MkA></template>
				</MkKeyValue>
				<MkKeyValue oneline>
					<template #key>{{ i18n.ts.createdAt }}</template>
					<template #value><MkTime :time="item.createdAt" mode="detail"/></template>
				</MkKeyValue>
				<MkKeyValue v-if="item.processedAt" oneline>
					<template #key>{{ i18n.ts._emojiApplication.processedAt }}</template>
					<template #value><MkTime :time="item.processedAt" mode="detail"/></template>
				</MkKeyValue>
				<!-- **却下理由が本体。** これが無いと「過去に却下された」しか分からない。 -->
				<MkInfo v-if="item.rejectReason" warn>{{ item.rejectReason }}</MkInfo>
			</div>
		</div>

		<!--
			**追加読み込みに失敗しても、読めていた履歴は消さない (レビュー L3)。**
			確認できていたものまで隠すと、判断材料が減る方向に倒れる。
		-->
		<MkInfo v-if="failed" warn>{{ i18n.ts._emojiApplication.relatedUnknown }}</MkInfo>
		<MkButton v-if="canLoadMore || failed" :disabled="fetching" @click="loadMore">
			{{ failed ? i18n.ts.retry : i18n.ts.loadMore }}
		</MkButton>
	</div>
</MkFolder>
</template>

<script lang="ts" setup>
import { computed, ref } from 'vue';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkLoading from '@/components/global/MkLoading.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkA from '@/components/global/MkA.vue';
import MkTime from '@/components/global/MkTime.vue';
import { i18n } from '@/i18n.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { canLoadMoreRelated, matchedByLabel, relatedImageMissingLabel, relatedNextCursor, relatedPreviewUrl, relatedStatusLabel, relatedSummaryLabel } from '@/utility/emoji-application-related.js';

type RelatedItem = {
	id: string;
	userId: string;
	status: 'pending' | 'approved' | 'rejected' | 'canceled';
	name: string;
	createdAt: string;
	processedAt: string | null;
	rejectReason?: string;
	url: string | null;
	remoteHost?: string;
	remoteName?: string;
	matchedBy: string[];
};

type Counts = {
	total: number;
	pending: number;
	approved: number;
	rejected: number;
	canceled: number;
};

const props = defineProps<{ applicationId: string }>();

const PAGE = 5;

const loaded = ref(false);
const counts = ref<Counts | null>(null);
const items = ref<RelatedItem[]>([]);
const fetching = ref(false);
const failed = ref(false);
const brokenPreviews = ref(new Set<string>());

const canLoadMore = computed(() => canLoadMoreRelated(counts.value, items.value.length));

// `counts` が null の分岐は型の narrowing 上どのみち要る。現状 `MkFolder` は
// `counts` がある時しか描画しないので到達しないが、空文字を返すとラベルの無い
// フォルダになるので、到達したときに読める既定ラベルを返す (レビュー R2-L3)。
const summaryLabel = computed(() => (counts.value == null ? i18n.ts._emojiApplication.related : relatedSummaryLabel(counts.value)));

// **1 件につき 1 回だけ解決する。** template から関数を呼ぶと再描画のたびに
// 走り、`<img>` の src が同値でも別インスタンスになる (親と同じ理由)。
const previewUrls = computed(() => {
	const map = new Map<string, string | null>();
	for (const item of items.value) {
		map.set(item.id, relatedPreviewUrl(item, brokenPreviews.value));
	}
	return map;
});

function onPreviewError(item: RelatedItem) {
	brokenPreviews.value = new Set(brokenPreviews.value).add(item.id);
}

async function fetchPage(untilId?: string) {
	if (fetching.value) return;
	fetching.value = true;
	try {
		const res = await misskeyApi('admin/emoji-application/related' as never, {
			applicationId: props.applicationId,
			limit: PAGE,
			untilId: untilId ?? null,
		} as never) as unknown as { counts: Counts; items: RelatedItem[] };
		counts.value = res.counts;
		items.value = untilId == null ? res.items : [...items.value, ...res.items];
		failed.value = false;
		loaded.value = true;
	} catch {
		// **握り潰さない。** 何も出さないと「履歴が無い」と読める。
		failed.value = true;
		// **初回が失敗しても loaded を立てる。** 立てないとプレースホルダのまま
		// で、警告も再試行のボタンも出ない。
		loaded.value = true;
	} finally {
		fetching.value = false;
	}
}

function loadMore() {
	// 初回が失敗しているときは最初から取り直す (untilId を渡すと 1 ページ目が
	// 永久に埋まらない)。
	void fetchPage(relatedNextCursor(items.value));
}

// **画面に入ったときに 1 回だけ取る (#2960)。** `v-appear` は throttle 付きで
// 何度でも呼ばれるので、取得済み / 取得中は弾く。
function onAppear() {
	if (loaded.value || fetching.value) return;
	void fetchPage();
}
</script>

<style lang="scss" module>
/* 取得中のローディング表示のぶん。**IntersectionObserver は高さ 0 でも交差を
 * 報告する**ので、外しても取得が止まるわけではない (レビュー R2-L2 で実測) */
.placeholder {
	min-height: 24px;
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
	width: 64px;
	height: 64px;
	display: grid;
	place-items: center;
	background: var(--MI_THEME-bg);
	border-radius: var(--MI-radius-sm);
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
	gap: 8px;
	align-items: center;
	flex-wrap: wrap;
}
.meta {
	display: flex;
	gap: 6px;
	flex-wrap: wrap;
}
.badge {
	font-size: 0.8em;
	padding: 2px 8px;
	border-radius: 999px;
	background: var(--MI_THEME-buttonBg);
}
.status {
	font-size: 0.9em;
	font-weight: bold;
}
.pending { color: var(--MI_THEME-warn); }
.approved { color: var(--MI_THEME-success); }
.rejected { color: var(--MI_THEME-error); }
.canceled { opacity: 0.7; }
</style>
