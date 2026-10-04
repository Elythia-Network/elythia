<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: DB の健全性 (#3095)。純正 backend にはこの endpoint が無い。
	判断材料を出すだけで、何も消さない。
-->
<template>
<div class="_gaps_m">
	<div :class="$style.caption">{{ i18n.ts._databaseHealth.description }}</div>
	<MkInfo v-if="error !== null" warn>{{ i18n.ts._databaseHealth.unavailable }} {{ error }}</MkInfo>
	<MkLoading v-else-if="report === null"/>
	<template v-else>
		<div :class="$style.caption">
			<div>{{ i18n.ts._databaseHealth.generatedAt }}: <MkTime :time="report.generatedAt" mode="detail"/></div>
			<div>
				{{ i18n.ts._databaseHealth.statsReset }}:
				<MkTime v-if="report.statsReset" :time="report.statsReset" mode="detail"/>
				<span v-else>{{ i18n.ts._databaseHealth.statsNeverReset }}</span>
			</div>
		</div>
		<MkInfo v-if="report.replicasConfigured" warn>{{ i18n.ts._databaseHealth.replicasNote }}</MkInfo>

		<MkFolder :defaultOpen="true">
			<template #label>{{ i18n.ts._databaseHealth.unusedIndexes }}</template>
			<template #suffix>{{ number(report.unusedIndexes.length) }}</template>
			<div class="_gaps_s">
				<div :class="$style.caption">{{ i18n.ts._databaseHealth.unusedIndexesCaption }}</div>
				<div :class="$style.caption">{{ i18n.ts._databaseHealth.statsResetCaption }}</div>
				<div v-if="report.unusedIndexes.length === 0" :class="$style.caption">{{ i18n.ts._databaseHealth.noUnusedIndexes }}</div>
				<div v-for="ix in report.unusedIndexes" :key="ix.table + '/' + ix.index" :class="$style.row">
					<span class="_monospace" :class="$style.name">{{ ix.index }}</span>
					<span class="_monospace" :class="$style.sub">{{ ix.table }}</span>
					<span :class="$style.sub">{{ bytes(ix.sizeBytes) }}</span>
					<span v-if="ix.unique || ix.primary" :class="$style.constraint">{{ i18n.ts._databaseHealth.constraint }}</span>
				</div>
			</div>
		</MkFolder>

		<MkFolder :defaultOpen="true">
			<template #label>{{ i18n.ts._databaseHealth.tables }}</template>
			<div class="_gaps_s">
				<div :class="$style.caption">{{ i18n.ts._databaseHealth.tablesCaption }}</div>
				<div v-for="t in report.tables" :key="t.table" :class="$style.row">
					<span class="_monospace" :class="$style.name">{{ t.table }}</span>
					<span v-if="t.vacuuming" :class="$style.sub">{{ i18n.ts._databaseHealth.vacuuming }}</span>
					<span :class="$style.sub">{{ t.sizeBytes === null ? i18n.ts._databaseHealth.sizeUnknown : bytes(t.sizeBytes) }}</span>
					<span :class="$style.sub">{{ i18n.tsx._databaseHealth.liveRows({ n: number(t.liveRows) }) }}</span>
					<span :class="$style.sub">{{ i18n.tsx._databaseHealth.deadRows({ n: number(t.deadRows), ratio: (t.deadRatio * 100).toFixed(1) }) }}</span>
					<span :class="$style.sub">{{ i18n.ts._databaseHealth.lastVacuum }}: <MkTime v-if="t.lastVacuum" :time="t.lastVacuum"/><template v-else>{{ i18n.ts._databaseHealth.never }}</template></span>
					<span :class="$style.sub">{{ i18n.ts._databaseHealth.lastAnalyze }}: <MkTime v-if="t.lastAnalyze" :time="t.lastAnalyze"/><template v-else>{{ i18n.ts._databaseHealth.never }}</template></span>
					<div v-for="p in problemsOf(t.table)" :key="p.kind" :class="$style.problem">{{ i18n.ts._databaseHealth.needsAttention }}: {{ p.kind === 'bloat' ? i18n.ts._databaseHealth.problemBloat : i18n.ts._databaseHealth.problemVacuum }}</div>
				</div>
			</div>
		</MkFolder>
	</template>
</div>
</template>

<script lang="ts" setup>
import { ref } from 'vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import bytes from '@/filters/bytes.js';
import number from '@/filters/number.js';

// mk-go: admin/database-health の応答。misskey-js の型に無いので手で持つ。
type Report = {
	generatedAt: string;
	statsReset: string | null;
	replicasConfigured: boolean;
	unusedIndexes: {
		table: string;
		index: string;
		scans: number;
		sizeBytes: number;
		unique: boolean;
		primary: boolean;
	}[];
	tables: {
		table: string;
		liveRows: number;
		deadRows: number;
		deadRatio: number;
		// 一度も VACUUM / ANALYZE されていないテーブルは null (0 と出すと空に見える)。
		sizeBytes: number | null;
		lastVacuum: string | null;
		lastAnalyze: string | null;
		modifiedSinceAnalyze: number;
		vacuuming: boolean;
	}[];
	// 肥大・VACUUM の遅れの判定結果。判定は backend だけが持つ (閾値を画面へ
	// 書き写すと self-check と食い違う)。
	problems: {
		table: string;
		kind: 'bloat' | 'vacuum';
		detail: string;
	}[];
};

const report = ref<Report | null>(null);
// 取得できなかったときのサーバーのエラーの文言。「非対応」とまとめて出すと、
// backend が古いのか、読み取りに失敗したのかも区別できない。
const error = ref<string | null>(null);

function problemsOf(table: string): Report['problems'] {
	return (report.value?.problems ?? []).filter(p => p.table === table);
}

async function fetchReport(): Promise<void> {
	try {
		// endpoint 名の cast は misskey-js の型に存在しないため (mk-go 独自)。
		report.value = await misskeyApi('admin/database-health' as never, {} as never) as unknown as Report;
	} catch (err) {
		error.value = (err as { message?: string } | null)?.message ?? String(err);
	}
}

fetchReport();
</script>

<style lang="scss" module>
.caption {
	font-size: 0.9em;
	opacity: 0.8;
}

.row {
	display: flex;
	flex-wrap: wrap;
	align-items: baseline;
	gap: 4px 12px;
	padding: 8px 0;
	border-bottom: solid 0.5px var(--MI_THEME-divider);
}

.name {
	font-weight: bold;
	word-break: break-all;
}

.sub {
	font-size: 0.85em;
	opacity: 0.8;
}

.constraint {
	font-size: 0.85em;
	color: var(--MI_THEME-warn);
}

.problem {
	flex-basis: 100%;
	font-size: 0.85em;
	color: var(--MI_THEME-warn);
}
</style>
