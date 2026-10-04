<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div class="_gaps">
	<MkInfo>{{ i18n.ts._driveUsage.databaseOnly }}</MkInfo>
	<!--
		**取得済みの数字と同時に出す。** 更新に失敗したときにここだけを描いて
		本体を消すと、前回の数字も「更新」ボタンも一緒に消えて再試行できなくなる。
	-->
	<MkInfo v-if="failed" warn>{{ i18n.ts._driveUsage.unavailable }}</MkInfo>

	<div v-if="loading" :class="$style.placeholder"><MkLoading/></div>

	<template v-else-if="usage">
		<div :class="$style.totals">
			<div :class="$style.total">
				<div :class="$style.totalLabel">{{ i18n.ts.total }}</div>
				<div :class="$style.totalValue">{{ bytes(usage.total.size) }}</div>
				<div :class="$style.totalSub">{{ i18n.tsx._driveUsage.files({ n: number(usage.total.count) }) }}</div>
			</div>
			<div :class="$style.total">
				<div :class="$style.totalLabel">{{ i18n.ts.local }}</div>
				<div :class="$style.totalValue">{{ bytes(usage.local.size) }}</div>
				<div :class="$style.totalSub">{{ i18n.tsx._driveUsage.files({ n: number(usage.local.count) }) }}</div>
			</div>
			<div v-if="hasRemoteBytes" :class="$style.total">
				<div :class="$style.totalLabel">{{ i18n.ts.remote }}</div>
				<div :class="$style.totalValue">{{ bytes(usage.remote.size) }}</div>
				<div :class="$style.totalSub">
					{{ i18n.tsx._driveUsage.files({ n: number(usage.remote.count) }) }}
					<span v-if="usage.remote.linkCount > 0">/ {{ i18n.tsx._driveUsage.linkOnly({ n: number(usage.remote.linkCount) }) }}</span>
				</div>
			</div>
		</div>

		<!--
			リモートに実体が無いときは欄ごと出さず 1 行で済ませる。**黙って消さない** —
			合計はリモートの件数を含むので、触れないと「合計とローカルが合わない」に
			なる。
		-->
		<div v-if="!hasRemoteBytes && usage.remote.count > 0" :class="$style.note">
			{{ i18n.tsx._driveUsage.remoteNoBytes({ n: number(usage.remote.count) }) }}
		</div>

		<MkFolder :defaultOpen="true">
			<template #label>{{ i18n.ts._driveUsage.byKind }}</template>
			<div class="_gaps_s">
				<div :class="$style.note">{{ i18n.ts._driveUsage.byKindNote }}</div>
				<div v-if="kindRows.length === 0" :class="$style.note">{{ i18n.ts._driveUsage.nothing }}</div>
				<div v-for="row in kindRows" :key="row.origin + '/' + row.kind" :class="$style.row">
					<!-- リモートを出していないときは全行が「(ローカル)」になるので添えない。 -->
					<span :class="$style.rowLabel">{{ kindLabel(row.kind) }}<template v-if="hasRemoteBytes"> ({{ row.origin === 'local' ? i18n.ts.local : i18n.ts.remote }})</template></span>
					<span :class="$style.rowValue">{{ bytes(row.size) }} / {{ i18n.tsx._driveUsage.files({ n: number(row.count) }) }}</span>
				</div>
			</div>
		</MkFolder>

		<MkFolder v-if="hasRemoteBytes" :defaultOpen="true">
			<template #label>{{ i18n.tsx._driveUsage.byHost({ n: number(usage.topLimit) }) }}</template>
			<div class="_gaps_s">
				<div v-if="usage.byHost.length === 0" :class="$style.note">{{ i18n.ts._driveUsage.nothing }}</div>
				<div v-for="row in usage.byHost" :key="row.host" :class="$style.row">
					<span :class="$style.rowLabel">{{ row.host }}</span>
					<span :class="$style.rowValue">{{ bytes(row.size) }} / {{ i18n.tsx._driveUsage.files({ n: number(row.count) }) }}</span>
				</div>
			</div>
		</MkFolder>

		<MkFolder :defaultOpen="true">
			<template #label>{{ i18n.tsx._driveUsage.byUser({ n: number(usage.topLimit) }) }}</template>
			<div class="_gaps_s">
				<div v-if="usage.byUser.length === 0" :class="$style.note">{{ i18n.ts._driveUsage.nothing }}</div>
				<div v-for="row in usage.byUser" :key="row.userId" :class="$style.row">
					<span :class="$style.rowLabel">@{{ row.username || row.userId }}</span>
					<span :class="$style.rowValue">{{ bytes(row.size) }} / {{ i18n.tsx._driveUsage.files({ n: number(row.count) }) }}</span>
				</div>
			</div>
		</MkFolder>

		<div :class="$style.footer">
			<span>{{ i18n.tsx._driveUsage.calculatedAt({ at: calculatedAtLabel, ms: number(usage.elapsedMs) }) }}</span>
			<MkButton small :disabled="refreshing" @click="refresh">{{ i18n.ts.reload }}</MkButton>
		</div>
	</template>

	<!-- 一度も取れていないとき。ここにも再試行の導線を残す。 -->
	<div v-else :class="$style.footer">
		<MkButton small :disabled="refreshing" @click="refresh">{{ i18n.ts.reload }}</MkButton>
	</div>
</div>
</template>

<script lang="ts" setup>
import { computed, onMounted, ref } from 'vue';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import bytes from '@/filters/bytes.js';
import number from '@/filters/number.js';
import { i18n } from '@/i18n.js';
import { dateString } from '@/filters/date.js';
import { misskeyApi } from '@/utility/misskey-api.js';

// mk-go 独自の admin/drive/usage (#3053)。純正 backend には無いので
// misskey-js の型にも載らない。overview.mkgo.vue と同じくローカル型で受ける。
type UsageBucket = {
	count: number;
	size: number;
	// linkCount は実体を持たない行 (isLink) の数。mk-go はリモートメディアを
	// キャッシュしないので、リモート側は count と一致し size は 0 になる。
	linkCount: number;
};

type DriveUsage = {
	calculatedAt: string;
	elapsedMs: number;
	cached: boolean;
	cacheTtlSeconds: number;
	topLimit: number;
	// source は数字の出所。今は 'database' だけで、DB が把握している量であって
	// object storage の実使用量ではないことを示す。
	source: string;
	total: UsageBucket;
	local: UsageBucket;
	remote: UsageBucket;
	byKind: (UsageBucket & { kind: string; origin: string })[];
	byHost: (UsageBucket & { host: string })[];
	byUser: (UsageBucket & { userId: string; username: string })[];
};

const usage = ref<DriveUsage | null>(null);
const loading = ref(true);
const refreshing = ref(false);
const failed = ref(false);

// **リモートは実体があるときだけ出す。** mk-go はリモートメディアをキャッシュしない
// ので、mk-go が作ったデータでは常に 0 バイト。件数だけの行を並べても「どこを消せば
// 効くか」の役に立たず、ローカルの内訳を押し下げるだけになる。純正 Misskey から
// 引き継いだ DB には実体のあるリモート行が残るので、そのときだけ従来どおり出す。
const hasRemoteBytes = computed(() => (usage.value?.remote.size ?? 0) > 0);

// 0 の行まで並べると 10 行の大半が空になるので、値のあるものだけ出す。
const kindRows = computed(() => (usage.value?.byKind ?? [])
	.filter(row => row.count > 0)
	.filter(row => hasRemoteBytes.value || row.origin === 'local'));

const calculatedAtLabel = computed(() => {
	if (usage.value == null) return '';
	return dateString(usage.value.calculatedAt);
});

function kindLabel(kind: string): string {
	switch (kind) {
		case 'attachment': return i18n.ts._driveUsage._kind.attachment;
		case 'avatar': return i18n.ts._driveUsage._kind.avatar;
		case 'banner': return i18n.ts._driveUsage._kind.banner;
		case 'emoji': return i18n.ts._driveUsage._kind.emoji;
		case 'other': return i18n.ts._driveUsage._kind.other;
		// サーバーが種類を増やしたとき、知らない値を落とすと合計と内訳が
		// 合わなくなる。そのまま出す。
		default: return kind;
	}
}

async function load(forceRecalc: boolean) {
	try {
		usage.value = await misskeyApi('admin/drive/usage' as never, { forceRecalc } as never) as unknown as DriveUsage;
		failed.value = false;
	} catch {
		// 純正 backend では endpoint ごと存在しない。集計に失敗したときも 500 が
		// 返る。**0 バイトとしては描かない** — 「使っていない」という誤った事実に
		// なるので、取れなかったことを出す。前回の数字は消さずに残す。
		failed.value = true;
	}
}

async function refresh() {
	refreshing.value = true;
	try {
		await load(true);
	} finally {
		refreshing.value = false;
	}
}

onMounted(async () => {
	await load(false);
	loading.value = false;
});
</script>

<style lang="scss" module>
.placeholder {
	padding: 32px 0;
	text-align: center;
}

.totals {
	display: grid;
	grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
	gap: 12px;
}

.total {
	padding: 16px;
	background: var(--MI_THEME-panel);
	border-radius: var(--MI-radius);
}

.totalLabel {
	font-size: 0.85em;
	opacity: 0.7;
}

.totalValue {
	font-size: 1.4em;
	font-weight: bold;
	margin-top: 4px;
}

.totalSub {
	font-size: 0.85em;
	opacity: 0.7;
	margin-top: 4px;
}

.note {
	opacity: 0.7;
	font-size: 0.85em;
}

.row {
	display: flex;
	gap: 8px;
	align-items: baseline;
	justify-content: space-between;
}

// ホスト名や利用者名は長くなりうる。MkKeyValue の oneline は key 側に
// 折り返しの指定が無く 30% の枠から溢れるので、独自の行にしてある。
.rowLabel {
	min-width: 0;
	overflow-wrap: anywhere;
}

.rowValue {
	flex-shrink: 0;
	white-space: nowrap;
	opacity: 0.85;
}

.footer {
	display: flex;
	align-items: center;
	justify-content: space-between;
	gap: 12px;
	flex-wrap: wrap;
	font-size: 0.85em;
	opacity: 0.8;
}
</style>
