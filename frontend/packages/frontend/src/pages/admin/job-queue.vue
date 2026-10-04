<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader v-model:tab="tab" :actions="headerActions" :tabs="headerTabs">
	<div class="_spacer">
		<div v-if="tab === '-'" class="_gaps">
			<div :class="$style.queues">
				<div v-for="q in queueInfos" :key="q.name" :class="$style.queue" @click="tab = (q.name as typeof tab)">
					<div style="display: flex; align-items: center; font-weight: bold;"><i class="ti ti-http-que" style="margin-right: 0.5em;"></i>{{ q.name }}<i v-if="!q.isPaused" style="color: var(--MI_THEME-success); margin-left: auto;" class="ti ti-player-play"></i></div>
					<div :class="$style.queueCounts">
						<MkKeyValue>
							<template #key>Active</template>
							<template #value>{{ kmg(q.counts.active, 2) }}</template>
						</MkKeyValue>
						<MkKeyValue>
							<template #key>Delayed</template>
							<template #value>{{ kmg(q.counts.delayed, 2) }}</template>
						</MkKeyValue>
						<MkKeyValue>
							<template #key>Waiting</template>
							<template #value>{{ kmg(q.counts.waiting, 2) }}</template>
						</MkKeyValue>
						<!--
							runtime は mk-go 独自の additive block。純正 backend では
							付かないので v-if で出し分ける。worker 数は auto-scale の
							有無に関わらず意味があるので常に出す。
						-->
						<MkKeyValue v-if="runtimeOf(q)">
							<template #key>Workers</template>
							<template #value>{{ workerLabel(runtimeOf(q)!) }}</template>
						</MkKeyValue>
					</div>
					<XChart :dataSet="{ completed: q.metrics.completed.data, failed: q.metrics.failed.data }"/>
				</div>
			</div>
		</div>
		<div v-else-if="queueInfo" class="_gaps">
			<MkFolder :defaultOpen="true">
				<template #label>Overview: {{ tab }}</template>
				<template #icon><i class="ti ti-http-que"></i></template>
				<template #suffix>#{{ queueInfo.db.processId }}:{{ queueInfo.db.port }} / {{ queueInfo.db.runId }}</template>
				<template #caption>{{ queueInfo.qualifiedName }}</template>
				<template #footer>
					<div class="_buttons">
						<MkButton rounded @click="promoteAllJobs"><i class="ti ti-player-track-next"></i> Promote all jobs</MkButton>
						<!-- <MkButton rounded @click="createJob"><i class="ti ti-plus"></i> Add job</MkButton> -->
						<MkButton v-if="queueInfo.isPaused" rounded @click="resumeQueue"><i class="ti ti-player-play"></i> Resume queue</MkButton>
						<MkButton v-else rounded danger @click="pauseQueue"><i class="ti ti-player-pause"></i> Pause queue</MkButton>
						<MkButton rounded danger @click="clearQueue"><i class="ti ti-trash"></i> Empty queue</MkButton>
					</div>
				</template>

				<div class="_gaps">
					<XChart :dataSet="{ completed: queueInfo.metrics.completed.data, failed: queueInfo.metrics.failed.data }" :aspectRatio="5"/>
					<div style="display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 12px;">
						<MkKeyValue>
							<template #key>Active</template>
							<template #value>{{ kmg(queueInfo.counts.active, 2) }}</template>
						</MkKeyValue>
						<MkKeyValue>
							<template #key>Delayed</template>
							<template #value>{{ kmg(queueInfo.counts.delayed, 2) }}</template>
						</MkKeyValue>
						<MkKeyValue>
							<template #key>Waiting</template>
							<template #value>{{ kmg(queueInfo.counts.waiting, 2) }}</template>
						</MkKeyValue>
					</div>
					<hr>
					<div style="display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 12px;">
						<MkKeyValue>
							<template #key>Clients: Connected</template>
							<template #value>{{ queueInfo.db.clients.connected }}</template>
						</MkKeyValue>
						<MkKeyValue>
							<template #key>Clients: Blocked</template>
							<template #value>{{ queueInfo.db.clients.blocked }}</template>
						</MkKeyValue>
						<MkKeyValue>
							<template #key>Memory: Peak</template>
							<template #value>{{ bytes(queueInfo.db.memory.peak, 1) }}</template>
						</MkKeyValue>
						<MkKeyValue>
							<template #key>Memory: Total</template>
							<template #value>{{ bytes(queueInfo.db.memory.total, 1) }}</template>
						</MkKeyValue>
						<MkKeyValue>
							<template #key>Memory: Used</template>
							<template #value>{{ bytes(queueInfo.db.memory.used, 1) }}</template>
						</MkKeyValue>
						<MkKeyValue>
							<template #key>Uptime</template>
							<template #value>{{ queueInfo.db.uptime }}</template>
						</MkKeyValue>
					</div>
					<!--
						mk-go の worker runtime (#2277)。Prometheus /metrics は無認証
						公開で LB ACL 必須なので admin からは読めない。その情報をここに出す。
						値はプロセスローカルかつ揮発 (再起動で消える) なので、恒久的な
						監視は Prometheus 側で行う前提。
					-->
					<template v-if="runtime">
						<hr>
						<div style="display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 12px;">
							<MkKeyValue>
								<template #key>Workers</template>
								<template #value>{{ workerLabel(runtime) }}</template>
							</MkKeyValue>
							<MkKeyValue>
								<template #key>Auto-scale</template>
								<template #value>{{ runtime.autoScale ? 'enabled' : 'disabled' }}</template>
							</MkKeyValue>
							<MkKeyValue>
								<template #key>Dispatch wait (p50/p95)</template>
								<template #value>{{ latencyLabel(runtime.dispatchWaitMs) }}</template>
							</MkKeyValue>
							<MkKeyValue>
								<template #key>Processing (p50/p95)</template>
								<template #value>{{ latencyLabel(runtime.processingMs) }}</template>
							</MkKeyValue>
							<MkKeyValue>
								<template #key>Recent failures</template>
								<template #value>{{ kmg(runtime.recentFailures, 2) }}</template>
							</MkKeyValue>
						</div>
						<template v-if="runtime.scaleEvents.length > 0">
							<hr>
							<div :class="$style.scaleEvents">
								<div v-for="(ev, i) in runtime.scaleEvents" :key="i" :class="$style.scaleEvent">
									<i :class="ev.direction === 'up' ? 'ti ti-arrow-up' : 'ti ti-arrow-down'" :style="{ color: ev.direction === 'up' ? 'var(--MI_THEME-success)' : 'var(--MI_THEME-warn)' }"></i>
									<span>{{ ev.from }} → {{ ev.to }}</span>
									<MkTime :time="ev.at" mode="relative" style="margin-left: auto; opacity: 0.7;"/>
								</div>
							</div>
						</template>
					</template>
				</div>
			</MkFolder>

			<MkFolder :defaultOpen="true" :withSpacer="false">
				<template #label>Jobs: {{ tab }}</template>
				<template #icon><i class="ti ti-list-check"></i></template>
				<template #suffix>&lt;A:{{ kmg(queueInfo.counts.active, 2) }}&gt; &lt;D:{{ kmg(queueInfo.counts.delayed, 2) }}&gt; &lt;W:{{ kmg(queueInfo.counts.waiting, 2) }}&gt;</template>
				<template #header>
					<MkTabs
						v-model:tab="jobState"
						:tabs="[{
							key: 'all',
							title: 'All',
							icon: 'ti ti-code-asterisk',
						}, {
							key: 'latest',
							title: 'Latest',
							icon: 'ti ti-logs',
						}, {
							key: 'completed',
							title: 'Completed',
							icon: 'ti ti-check',
						}, {
							key: 'failed',
							title: 'Failed',
							icon: 'ti ti-circle-x',
						}, {
							key: 'active',
							title: 'Active',
							icon: 'ti ti-player-play',
						}, {
							key: 'delayed',
							title: 'Delayed',
							icon: 'ti ti-clock',
						}, {
							key: 'wait',
							title: 'Waiting',
							icon: 'ti ti-hourglass-high',
						}]"
					/>
				</template>
				<template #footer>
					<div class="_buttons">
						<MkButton rounded @click="fetchJobs()"><i class="ti ti-reload"></i> Refresh view</MkButton>
						<MkButton rounded danger style="margin-left: auto;" @click="removeJobs"><i class="ti ti-trash"></i> Remove jobs</MkButton>
					</div>
				</template>

				<div class="_spacer">
					<MkInput
						v-model="searchQuery"
						:placeholder="i18n.ts.search"
						type="search"
						style="margin-bottom: 16px;"
					>
						<template #prefix><i class="ti ti-search"></i></template>
					</MkInput>

					<MkLoading v-if="jobsFetching"/>
					<MkTl
						v-else
						:events="jobs.map((job) => ({
							id: job.id,
							timestamp: job.finishedOn ?? job.processedOn ?? job.timestamp,
							data: job,
						}))"
						groupBy="h"
						class="_monospace"
					>
						<template #right="{ event: job }">
							<XJob :job="job" :queueType="tab" style="margin: 4px 0;" @needRefresh="refreshJob(job.id)"/>
						</template>
					</MkTl>
				</div>
			</MkFolder>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { ref, computed, watch } from 'vue';
import * as Misskey from 'misskey-js';
import { debounce } from 'throttle-debounce';
import { useInterval } from '@@/js/use-interval.js';
import XChart from './job-queue.chart.vue';
import XJob from './job-queue.job.vue';
import * as os from '@/os.js';
import { i18n } from '@/i18n.js';
import { definePage } from '@/page.js';
import MkButton from '@/components/MkButton.vue';
import { misskeyApi } from '@/utility/misskey-api.js';
import MkTabs from '@/components/MkTabs.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkTl from '@/components/MkTl.vue';
import kmg from '@/filters/kmg.js';
import MkInput from '@/components/MkInput.vue';
import bytes from '@/filters/bytes.js';

const tab = ref<typeof Misskey.queueTypes[number] | '-'>('-');

// misskey-js の autogen 型は upstream backend の OpenAPI 由来なので、`queue`
// パラメータが純正の 10 queue 名に固定されている。mk-go は別構成
// (Misskey.queueTypes を書き換え済み) で名前が一致しないため、API 呼び出し時に
// ここで一度だけキャストする。autogen を編集すると upstream 追従で失われるうえ、
// mk-go では再生成もできない (生成に NestJS backend が要る)。
type ApiQueueName = Misskey.Endpoints['admin/queue/queue-stats']['req']['queue'];
const apiQueue = computed(() => tab.value as unknown as ApiQueueName);
const jobState = ref<Misskey.entities.AdminQueueJobsRequest['state'][number] | 'all' | 'latest'>('all');
const jobs = ref<Misskey.entities.QueueJob[]>([]);
const jobsFetching = ref(true);
const queueInfos = ref<Misskey.entities.AdminQueueQueuesResponse>([]);
const queueInfo = ref<Misskey.entities.AdminQueueQueueStatsResponse | null>(null);
const searchQuery = ref('');

// mk-go 独自の worker runtime block (#2277)。純正 backend の応答には無いので
// optional 扱いにし、無ければ該当 UI を丸ごと出さない。autogen 型にも無いため
// ここで型を定義する (autogen 再生成で消えないように)。
type QueueLatency = { count: number; p50: number; p95: number; max: number };
type QueueRuntime = {
	workers: number;
	minWorkers: number;
	maxWorkers: number;
	autoScale: boolean;
	dispatchWaitMs: QueueLatency;
	processingMs: QueueLatency;
	recentFailures: number;
	scaleEvents: { at: string; direction: 'up' | 'down'; from: number; to: number }[];
};

function runtimeOf(q: unknown): QueueRuntime | null {
	return (q as { runtime?: QueueRuntime } | null)?.runtime ?? null;
}

const runtime = computed<QueueRuntime | null>(() => runtimeOf(queueInfo.value));

// 「6 (4–32)」形式。auto-scale 無効時は範囲を出さない (静的 concurrency なので
// 範囲に意味が無い)。
function workerLabel(rt: QueueRuntime): string {
	if (!rt.autoScale) return String(rt.workers);
	return `${rt.workers} (${rt.minWorkers}–${rt.maxWorkers})`;
}

// 「12.3ms / 88.0ms」形式。サンプルが無い窓は "-" にする (0ms と紛らわしいため)。
function latencyLabel(l: QueueLatency): string {
	if (l.count === 0) return '-';
	return `${formatMs(l.p50)} / ${formatMs(l.p95)}`;
}

function formatMs(ms: number): string {
	if (ms >= 1000) return `${(ms / 1000).toFixed(2)}s`;
	return `${ms.toFixed(1)}ms`;
}

async function fetchQueues() {
	if (tab.value !== '-') return;
	queueInfos.value = await misskeyApi('admin/queue/queues');
}

async function fetchCurrentQueue() {
	if (tab.value === '-') return;
	queueInfo.value = await misskeyApi('admin/queue/queue-stats', { queue: apiQueue.value });
}

async function fetchJobs() {
	if (tab.value === '-') return;
	jobsFetching.value = true;
	const state = jobState.value;
	jobs.value = await misskeyApi('admin/queue/jobs', {
		queue: apiQueue.value,
		state: state === 'all' ? ['completed', 'failed', 'active', 'delayed', 'wait'] : state === 'latest' ? ['completed', 'failed'] : [state],
		search: searchQuery.value.trim() === '' ? undefined : searchQuery.value,
	}).then((res: Misskey.entities.AdminQueueJobsResponse) => {
		if (state === 'all') {
			res.sort((a, b) => (a.processedOn ?? a.timestamp) > (b.processedOn ?? b.timestamp) ? -1 : 1);
		} else if (state === 'latest') {
			res.sort((a, b) => a.processedOn! > b.processedOn! ? -1 : 1);
		} else if (state === 'delayed') {
			res.sort((a, b) => (a.processedOn ?? a.timestamp) > (b.processedOn ?? b.timestamp) ? -1 : 1);
		}
		return res;
	});
	jobsFetching.value = false;
}

watch([tab], async () => {
	if (tab.value === '-') {
		fetchQueues();
	} else {
		fetchCurrentQueue();
		fetchJobs();
	}
}, { immediate: true });

watch([jobState], () => {
	fetchJobs();
});

const search = debounce(1000, () => {
	fetchJobs();
});

watch([searchQuery], () => {
	search();
});

useInterval(() => {
	if (tab.value === '-') {
		fetchQueues();
	} else {
		fetchCurrentQueue();
	}
}, 1000 * 10, {
	immediate: false,
	afterMounted: true,
});

async function clearQueue() {
	if (tab.value === '-') return;

	const { canceled } = await os.confirm({
		type: 'warning',
		title: i18n.ts.areYouSure,
	});
	if (canceled) return;

	os.apiWithDialog('admin/queue/clear', { queue: apiQueue.value, state: '*' });

	fetchCurrentQueue();
	fetchJobs();
}

async function promoteAllJobs() {
	if (tab.value === '-') return;

	const { canceled } = await os.confirm({
		type: 'warning',
		title: i18n.ts.areYouSure,
	});
	if (canceled) return;

	os.apiWithDialog('admin/queue/promote-jobs', { queue: apiQueue.value });

	fetchCurrentQueue();
	fetchJobs();
}

async function pauseQueue() {
	if (tab.value === '-') return;

	const { canceled } = await os.confirm({
		type: 'warning',
		title: i18n.ts.areYouSure,
	});
	if (canceled) return;

	await os.apiWithDialog('admin/queue/pause', { queue: apiQueue.value });

	fetchCurrentQueue();
	fetchJobs();
}

async function resumeQueue() {
	if (tab.value === '-') return;

	await os.apiWithDialog('admin/queue/resume', { queue: apiQueue.value });

	fetchCurrentQueue();
	fetchJobs();
}

async function removeJobs() {
	if (tab.value === '-' || jobState.value === 'latest') return;

	const { canceled } = await os.confirm({
		type: 'warning',
		title: i18n.ts.areYouSure,
	});
	if (canceled) return;

	os.apiWithDialog('admin/queue/clear', { queue: apiQueue.value, state: jobState.value === 'all' ? '*' : jobState.value });

	fetchCurrentQueue();
	fetchJobs();
}

async function refreshJob(jobId: string) {
	if (tab.value === '-') return;
	const newJob = await misskeyApi('admin/queue/show-job', { queue: apiQueue.value, jobId });
	const index = jobs.value.findIndex((job) => job.id === jobId);
	if (index !== -1) {
		jobs.value[index] = newJob;
	}
}

const headerActions = computed(() => []);

const headerTabs = computed<{
	key: string;
	title: string;
	icon?: string;
}[]>(() => [{
	key: '-',
	title: i18n.ts.jobQueue,
	icon: 'ti ti-list-check',
}, ...Misskey.queueTypes.map((q) => ({
	key: q,
	title: q,
}))]);

definePage(() => ({
	title: i18n.ts.jobQueue,
	icon: 'ti ti-clock-play',
	needWideArea: true,
}));
</script>

<style lang="scss" module>
.queues {
	display: grid;
	grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
	gap: 14px;
}

.queue {
	padding: 14px 18px;
	background-color: var(--MI_THEME-panel);
	border-radius: 8px;
	cursor: pointer;
}

.scaleEvents {
	display: flex;
	flex-direction: column;
	gap: 6px;
	max-height: 200px;
	overflow-y: auto;
}

.scaleEvent {
	display: flex;
	align-items: center;
	gap: 8px;
	font-size: 0.9em;
}

.queueCounts {
	display: grid;
	grid-template-columns: repeat(auto-fill, minmax(80px, 1fr));
	gap: 8px;
	font-size: 85%;
	margin: 6px 0;
}
</style>
