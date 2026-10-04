<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<MkFolder>
	<template #label>
		<span v-if="job.opts.repeat != null" style="margin-right: 1em;">&lt;repeat&gt;</span>
		<span v-else style="margin-right: 1em;">#{{ job.id }}</span>
		<span>{{ job.name }}</span>
	</template>
	<template #suffix>
		<MkTime :time="job.finishedOn ?? job.processedOn ?? job.timestamp" mode="relative"/>
		<span v-if="job.progress != null && typeof job.progress === 'number' && job.progress > 0" style="margin-left: 1em;">{{ Math.floor(job.progress) }}%</span>
		<span v-if="job.opts.attempts != null && job.opts.attempts > 0 && job.attempts > 1" style="margin-left: 1em; color: var(--MI_THEME-warn); font-variant-numeric: diagonal-fractions;">{{ job.attempts }}/{{ job.opts.attempts }}</span>
		<span v-if="job.isFailed && job.finishedOn != null" style="margin-left: 1em; color: var(--MI_THEME-error)"><i class="ti ti-circle-x"></i></span>
		<span v-else-if="job.isFailed" style="margin-left: 1em; color: var(--MI_THEME-warn)"><i class="ti ti-alert-triangle"></i></span>
		<span v-else-if="job.finishedOn != null" style="margin-left: 1em; color: var(--MI_THEME-success)"><i class="ti ti-check"></i></span>
		<span v-else-if="job.delay != null && job.delay != 0" style="margin-left: 1em;"><i class="ti ti-clock"></i></span>
		<span v-else-if="job.processedOn != null" style="margin-left: 1em; color: var(--MI_THEME-success)"><i class="ti ti-player-play"></i></span>
	</template>
	<template #header>
		<MkTabs
			v-model:tab="tab"
			:tabs="[{
					key: 'info',
					title: 'Info',
					icon: 'ti ti-info-circle',
				}, {
					key: 'timeline',
					title: 'Timeline',
					icon: 'ti ti-timeline-event',
				}, {
					key: 'data',
					title: 'Data',
					icon: 'ti ti-package',
				}, ...(canEdit ? [{
					key: 'dataEdit',
					title: 'Data (edit)',
					icon: 'ti ti-package',
				}] : []),
				...(job.returnValue != null ? [{
					key: 'result',
					title: 'Result',
					icon: 'ti ti-check',
				}] : []),
				...(job.stacktrace.length > 0 ? [{
					key: 'error',
					title: 'Error',
					icon: 'ti ti-alert-triangle',
				}] : []), {
					key: 'logs',
					title: 'Logs',
					icon: 'ti ti-logs',
				}]"
		/>
	</template>
	<template #footer>
		<div class="_buttons">
			<MkButton rounded @click="copyRaw()"><i class="ti ti-copy"></i> Copy raw</MkButton>
			<MkButton rounded @click="refresh()"><i class="ti ti-reload"></i> Refresh view</MkButton>
			<MkButton rounded @click="promoteJob()"><i class="ti ti-player-track-next"></i> Promote</MkButton>
			<!-- <MkButton rounded @click="moveJob"><i class="ti ti-arrow-right"></i> Move to</MkButton> -->
			<MkButton danger rounded style="margin-left: auto;" @click="removeJob()"><i class="ti ti-trash"></i> Remove</MkButton>
		</div>
	</template>

	<div v-if="tab === 'info'" class="_gaps_s">
		<div style="display: grid; grid-template-columns: repeat(auto-fill, minmax(250px, 1fr)); gap: 12px;">
			<MkKeyValue>
				<template #key>ID</template>
				<template #value>{{ job.id }}</template>
			</MkKeyValue>
			<MkKeyValue>
				<template #key>Created at</template>
				<template #value><MkTime :time="job.timestamp" mode="detail"/></template>
			</MkKeyValue>
			<MkKeyValue v-if="job.processedOn != null">
				<template #key>Processed at</template>
				<template #value><MkTime :time="job.processedOn" mode="detail"/></template>
			</MkKeyValue>
			<MkKeyValue v-if="job.finishedOn != null">
				<template #key>Finished at</template>
				<template #value><MkTime :time="job.finishedOn" mode="detail"/></template>
			</MkKeyValue>
			<MkKeyValue v-if="job.processedOn != null && job.finishedOn != null">
				<template #key>Spent</template>
				<template #value>{{ job.finishedOn - job.processedOn }}ms</template>
			</MkKeyValue>
			<MkKeyValue v-if="job.failedReason != null">
				<template #key>Failed reason</template>
				<template #value><i style="color: var(--MI_THEME-error)" class="ti ti-alert-triangle"></i> {{ job.failedReason }}</template>
			</MkKeyValue>
			<MkKeyValue v-if="job.opts.attempts != null && job.opts.attempts > 0">
				<template #key>Attempts</template>
				<template #value>{{ job.attempts }} of {{ job.opts.attempts }}</template>
			</MkKeyValue>
			<MkKeyValue v-if="job.progress != null && typeof job.progress === 'number' && job.progress > 0">
				<template #key>Progress</template>
				<template #value>{{ Math.floor(job.progress) }}%</template>
			</MkKeyValue>
		</div>
		<MkFolder :withSpacer="false">
			<template #label>Options</template>
			<MkCode :code="JSON5.stringify(job.opts, null, '\t')" lang="js"/>
		</MkFolder>
	</div>
	<div v-else-if="tab === 'timeline'">
		<MkTl :events="timeline" groupBy="h">
			<template #left="{ event }">
				<div>
					<template v-if="event.type === 'finished'">
						<template v-if="job.isFailed">
							<b>Finished</b> <i class="ti ti-circle-x" style="color: var(--MI_THEME-error);"></i>
						</template>
						<template v-else>
							<b>Finished</b> <i class="ti ti-check" style="color: var(--MI_THEME-success);"></i>
						</template>
					</template>
					<template v-else-if="event.type === 'processed'">
						<b>Processed</b> <i class="ti ti-player-play"></i>
						<span v-if="event.attempt > 1" style="margin-left: 0.5em; color: var(--MI_THEME-warn); font-variant-numeric: diagonal-fractions;">#{{ event.attempt }}</span>
					</template>
					<template v-else-if="event.type === 'attempt'">
						<b>Attempt #{{ event.attempt }}</b> <i class="ti ti-alert-triangle" style="color: var(--MI_THEME-warn);"></i>
					</template>
					<template v-else-if="event.type === 'created'">
						<b>Created</b> <i class="ti ti-plus"></i>
					</template>
				</div>
			</template>
			<template #right="{ event, timestamp, delta }">
				<div style="margin: 8px 0;">
					<div>at <MkTime :time="timestamp" mode="detail"/></div>
					<div style="font-size: 90%; opacity: 0.7;">{{ timestamp }} (+{{ msSMH(delta) }})</div>
				</div>
			</template>
		</MkTl>
	</div>
	<div v-else-if="tab === 'data'">
		<MkCode :code="JSON5.stringify(job.data, null, '\t')" lang="js"/>
	</div>
	<div v-else-if="tab === 'dataEdit'" class="_gaps_s">
		<MkCodeEditor v-model="editData" lang="json5"></MkCodeEditor>
		<MkButton><i class="ti ti-device-floppy"></i> Update</MkButton>
	</div>
	<div v-else-if="tab === 'result'">
		<MkCode :code="JSON5.stringify(job.returnValue, null, '\t')" lang="json5"/>
	</div>
	<div v-else-if="tab === 'error'" class="_gaps_s">
		<MkCode v-for="log in job.stacktrace" :code="log" lang="stacktrace"/>
	</div>
	<div v-else-if="tab === 'logs'">
		<MkButton primary rounded @click="loadLogs()"><i class="ti ti-refresh"></i> Load logs</MkButton>
		<div v-for="log in logs">{{ log }}</div>
	</div>
</MkFolder>
</template>

<script lang="ts" setup>
import { ref, computed } from 'vue';
import * as Misskey from 'misskey-js';
import JSON5 from 'json5';
import type { TlEvent } from '@/components/MkTl.vue';
import * as os from '@/os.js';
import { i18n } from '@/i18n.js';
import MkButton from '@/components/MkButton.vue';
import MkTabs from '@/components/MkTabs.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkCode from '@/components/MkCode.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkCodeEditor from '@/components/MkCodeEditor.vue';
import MkTl from '@/components/MkTl.vue';
import { copyToClipboard } from '@/utility/copy-to-clipboard.js';

function msSMH(v: number | null) {
	if (v == null) return 'N/A';
	if (v === 0) return '0';
	const suffixes = ['ms', 's', 'm', 'h'];
	const isMinus = v < 0;
	if (isMinus) v = -v;
	const i = Math.floor(Math.log(v) / Math.log(1000));
	const value = v / Math.pow(1000, i);
	const suffix = suffixes[i];
	return `${isMinus ? '-' : ''}${value.toFixed(1)}${suffix}`;
}

const props = defineProps<{
	job: Misskey.entities.QueueJob;
	queueType: typeof Misskey.queueTypes[number];
}>();

// job-queue.vue と同じ理由のキャスト (autogen 型が純正の queue 名に固定)。
type ApiQueueName = Misskey.Endpoints['admin/queue/retry-job']['req']['queue'];
const apiQueue = computed(() => props.queueType as unknown as ApiQueueName);

const emit = defineEmits<{
	(ev: 'needRefresh'): void;
}>();

const tab = ref('info');
const editData = ref(JSON5.stringify(props.job.data, null, '\t'));
const canEdit = true;
const logs = ref<string[]>([]);

type TlType = TlEvent<{
	type: 'created' | 'finished';
} | {
	type: 'processed' | 'attempt';
	// attempt は「何回目の実行か」(Bull の attemptsMade)。
	attempt: number;
}>;

// attemptsAt は mk-go 独自の additive field (#2692)。純正 backend や、記録が
// 入る前に失敗した job では無い。misskey-js の型は upstream の OpenAPI から
// 生成しているのでここには現れない — runtime (#2277) と同じく局所的に受ける。
function attemptsAtOf(job: unknown): number[] {
	const v = (job as { attemptsAt?: unknown } | null)?.attemptsAt;
	return Array.isArray(v) ? v.filter((x): x is number => typeof x === 'number') : [];
}

const timeline = computed(() => {
	const events: TlType[] = [{
		id: 'created',
		timestamp: props.job.timestamp,
		data: {
			type: 'created',
		},
	}];

	// **試行ごとの行は記録がある場合だけ出す。** Bull / BullMQ は attempt ごとの
	// 時刻を保存しないので、upstream は `timestamp + i` という架空の値を割り当てて
	// 「作成直後」に全部を積み、表示だけ `at ?` にしていた。時系列として嘘になり
	// delta 表示も無意味だった。
	//
	// mk-go backend は mkq の拡張で実際の開始時刻を返す (`attemptsAt`)。ある分は
	// 実時刻で並べ、無い job (記録が入る前に失敗したもの / 純正 backend) は
	// 回数だけを Processed 行に添える。
	const attemptsAt = attemptsAtOf(props.job);
	for (let i = 0; i < attemptsAt.length; i++) {
		// 最後の試行は Processed 行と同じ時刻なので重ねない。
		if (props.job.processedOn != null && attemptsAt[i] === props.job.processedOn) continue;
		events.push({
			id: `attempt-${i}`,
			timestamp: attemptsAt[i],
			data: {
				type: 'attempt',
				attempt: i + 1,
			},
		});
	}
	if (props.job.processedOn != null) {
		events.push({
			id: 'processed',
			timestamp: props.job.processedOn,
			data: {
				type: 'processed',
				attempt: props.job.attempts,
			},
		});
	}
	if (props.job.finishedOn != null) {
		events.push({
			id: 'finished',
			timestamp: props.job.finishedOn,
			data: {
				type: 'finished',
			},
		});
	}
	return events;
});

async function promoteJob() {
	const { canceled } = await os.confirm({
		type: 'warning',
		title: i18n.ts.areYouSure,
	});
	if (canceled) return;

	os.apiWithDialog('admin/queue/retry-job', { queue: apiQueue.value, jobId: props.job.id });
}

async function removeJob() {
	const { canceled } = await os.confirm({
		type: 'warning',
		title: i18n.ts.areYouSure,
	});
	if (canceled) return;

	os.apiWithDialog('admin/queue/remove-job', { queue: apiQueue.value, jobId: props.job.id });
}

async function loadLogs() {
	logs.value = await os.apiWithDialog('admin/queue/show-job-logs', { queue: apiQueue.value, jobId: props.job.id });
}

// TODO
// function moveJob() {
//
// }

function refresh() {
	emit('needRefresh');
}

function copyRaw() {
	const raw = JSON.stringify(props.job, null, '\t');
	copyToClipboard(raw);
}
</script>

<style lang="scss" module>

</style>
