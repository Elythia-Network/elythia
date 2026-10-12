<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div class="_gaps_m" style="padding: 16px;">
	<div v-if="error" :class="$style.error">{{ error }}</div>

	<template v-else-if="overview">
		<div class="_gaps_s">
			<div :class="$style.counts">
				<div><b>{{ overview.counts.total }}</b> 判定済み</div>
				<div><b>{{ overview.counts.granted }}</b> 付与済み</div>
				<div><b>{{ overview.counts.held }}</b> 保留</div>
				<div :class="overview.counts.failing > 0 ? $style.bad : undefined">
					<b>{{ overview.counts.failing }}</b> 失敗
				</div>
			</div>
			<!--
				失敗が出るのは主体の管理者が使えなくなったときが主。原因が
				分からないまま「昇格が止まっている」になるのを避ける。
			-->
			<div v-if="overview.counts.failing > 0" :class="$style.note">
				付与に失敗した利用者があります。実行主体の管理者 ({{ overview.config.actorId }}) が
				凍結・降格・削除されていないか確認してください。
			</div>
		</div>

		<MkFolder :defaultOpen="true">
			<template #label>条件</template>
			<div class="_gaps_s" style="padding: 12px;">
				<div>付与するロール: <code>{{ overview.config.roleId }}</code></div>
				<div>実行主体: <code>{{ overview.config.actorId }}</code></div>
				<div>作成から {{ overview.config.minAccountAgeDays }} 日以上</div>
				<div>ノート {{ overview.config.minNotes }} 件以上</div>
				<div>実行間隔: <code>{{ overview.config.cron }}</code></div>
			</div>
		</MkFolder>

		<MkFolder :defaultOpen="true">
			<template #label>直近の実行</template>
			<template #suffix>{{ overview.runs.length }} 件</template>
			<div style="overflow-x: auto;">
				<table :class="$style.table">
					<thead>
						<tr><th>開始</th><th>走査</th><th>付与</th><th>失敗</th><th>所要</th><th>エラー</th></tr>
					</thead>
					<tbody>
						<tr v-for="(run, i) in overview.runs" :key="i">
							<td>{{ new Date(run.startedAt).toLocaleString() }}</td>
							<td>{{ run.scanned }}</td>
							<td>{{ run.granted }}</td>
							<td :class="run.failed > 0 ? $style.bad : undefined">{{ run.failed }}</td>
							<td>{{ run.elapsedMs }} ms</td>
							<td :class="$style.err">{{ run.error }}</td>
						</tr>
					</tbody>
				</table>
			</div>
		</MkFolder>

		<MkFolder :defaultOpen="false">
			<template #label>判定状態</template>
			<div class="_gaps_s" style="padding: 12px;">
				<div class="_gaps_s" style="display: flex; gap: 8px; flex-wrap: wrap;">
					<MkButton v-for="f in filters" :key="f.value" :primary="filter === f.value" inline @click="setFilter(f.value)">
						{{ f.label }}
					</MkButton>
				</div>
				<div v-if="subjects.length === 0" :class="$style.note">該当する利用者はいません。</div>
				<div style="overflow-x: auto;">
					<table v-if="subjects.length > 0" :class="$style.table">
						<thead>
							<tr><th>ユーザー</th><th>状態</th><th>理由</th><th>評価</th><th></th></tr>
						</thead>
						<tbody>
							<tr v-for="s in subjects" :key="s.userId">
								<td><code>{{ s.userId }}</code></td>
								<td>
									<span v-if="s.held">保留</span>
									<span v-else-if="s.granted">付与済み</span>
									<span v-else>未付与</span>
								</td>
								<td>
									{{ s.reason }}
									<div v-if="s.lastError" :class="$style.err">{{ s.lastError }}</div>
								</td>
								<td>{{ s.evaluatedAt ? new Date(s.evaluatedAt).toLocaleString() : '-' }}</td>
								<td>
									<MkButton inline @click="toggleHold(s)">
										{{ s.held ? '保留を解除' : '保留にする' }}
									</MkButton>
								</td>
							</tr>
						</tbody>
					</table>
				</div>
			</div>
		</MkFolder>
	</template>

	<MkLoading v-else/>
</div>
</template>

<script lang="ts" setup>
import { ref, onMounted } from 'vue';
import { MkLoading, MkButton, MkFolder } from '@/plugin-api.js';
import { api, type Overview, type Subject } from './api.js';

const overview = ref<Overview | null>(null);
const subjects = ref<Subject[]>([]);
const filter = ref('');
const error = ref('');

const filters = [
	{ label: 'すべて', value: '' },
	{ label: '付与済み', value: 'granted' },
	{ label: '保留', value: 'held' },
	{ label: '失敗', value: 'failing' },
];

function message(err: unknown): string {
	// 権限が無い場合もここに来る。バックエンドが返した理由を見せる。
	return (err as { message?: string } | null)?.message ?? '取得に失敗しました';
}

async function loadSubjects() {
	try {
		const res = await api<{ subjects: Subject[] }>('admin/subjects', { filter: filter.value });
		subjects.value = res.subjects;
	} catch (err) {
		error.value = message(err);
	}
}

function setFilter(value: string) {
	filter.value = value;
	loadSubjects();
}

async function toggleHold(s: Subject) {
	try {
		await api('admin/hold', { userId: s.userId, held: !s.held });
		await Promise.all([load(), loadSubjects()]);
	} catch (err) {
		error.value = message(err);
	}
}

async function load() {
	try {
		overview.value = await api<Overview>('admin/overview');
	} catch (err) {
		error.value = message(err);
	}
}

onMounted(async () => {
	await load();
	await loadSubjects();
});
</script>

<style lang="scss" module>
.counts {
	display: flex;
	gap: 16px;
	flex-wrap: wrap;
}

.bad {
	color: var(--MI_THEME-error);
}

.note {
	font-size: 0.9em;
	opacity: 0.8;
}

.table {
	width: 100%;
	border-collapse: collapse;
	font-size: 0.9em;

	th, td {
		text-align: left;
		padding: 6px 10px;
		border-bottom: 1px solid var(--MI_THEME-divider);
		white-space: nowrap;
	}
}

.err {
	color: var(--MI_THEME-error);
	white-space: normal;
	font-size: 0.9em;
}
</style>
