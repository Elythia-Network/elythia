<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div v-if="metrics" :class="$style.root">
	<div class="_table status">
		<div class="_row">
			<div class="_cell"><div class="_label">Goroutines</div>{{ number(metrics.go.goroutines) }}</div>
			<div class="_cell"><div class="_label">Heap</div>{{ bytes(metrics.go.heapAllocBytes) }}</div>
			<div class="_cell"><div class="_label">GC</div>{{ number(metrics.go.gcNum) }}</div>
			<div class="_cell"><div class="_label">Uptime</div>{{ uptimeLabel }}</div>
		</div>
	</div>

	<div class="charts">
		<div class="chart">
			<div class="title">Goroutines</div>
			<MkMiniChart v-if="goroutineHistory.length > 1" class="graph" :src="goroutineHistory"/>
		</div>
		<div class="chart">
			<div class="title">Heap</div>
			<MkMiniChart v-if="heapHistory.length > 1" class="graph" :src="heapHistory"/>
		</div>
	</div>

	<div :class="$style.version">mk-go {{ metrics.version.mkGo }} / Misskey {{ metrics.version.misskey }}</div>
</div>
</template>

<script lang="ts" setup>
import { computed, onMounted, onUnmounted, ref } from 'vue';
import MkMiniChart from '@/components/MkMiniChart.vue';
import number from '@/filters/number.js';
import bytes from '@/filters/bytes.js';
import { misskeyApi } from '@/utility/misskey-api.js';

// mk-go 独自の admin/server-metrics (#2395)。純正 backend には無いので
// misskey-js の型にも載らない。admin/job-queue.vue の QueueRuntime と同じく
// ローカル型で受ける。
type ServerMetrics = {
	uptimeMs: number;
	version: { misskey: string; mkGo: string };
	go: {
		goroutines: number;
		gomaxprocs: number;
		heapAllocBytes: number;
		heapSysBytes: number;
		heapObjects: number;
		gcNum: number;
		lastGcPauseNs: number;
		gcCpuFraction: number;
	};
};

// 推移はサーバーに残さずクライアント側のリングバッファで持つ。30 点 = 5 分。
const HISTORY_LEN = 30;
// ReadMemStats は stop-the-world を伴うので、短い間隔で叩くと観測が観測対象を
// 汚す。10s なら実用上の鮮度と両立する。
const POLL_INTERVAL_MS = 10_000;

const metrics = ref<ServerMetrics | null>(null);
const goroutineHistory = ref<number[]>([]);
const heapHistory = ref<number[]>([]);

let timer: number | null = null;
// onMounted の初回 fetch を await している最中に画面を離れると、onUnmounted が
// 先に走って timer が null のまま interval が張られ、以後止められなくなる。
// 破棄済みかどうかを明示的に持って防ぐ。
let disposed = false;

const uptimeLabel = computed(() => {
	if (metrics.value == null) return '';
	const totalSec = Math.floor(metrics.value.uptimeMs / 1000);
	const d = Math.floor(totalSec / 86400);
	const h = Math.floor((totalSec % 86400) / 3600);
	const m = Math.floor((totalSec % 3600) / 60);
	if (d > 0) return `${d}d ${h}h`;
	if (h > 0) return `${h}h ${m}m`;
	return `${m}m`;
});

// MkMiniChart は描画前に src を reverse するため、**新しい順** (index 0 が最新) の
// 配列を期待する。head の丸印も src[0] の位置に打たれる。古い順で渡すと時間軸が
// 左右反転して描かれるので、先頭に積んで末尾を捨てる。
function push(buf: number[], value: number): number[] {
	const next = [value, ...buf];
	return next.length > HISTORY_LEN ? next.slice(0, HISTORY_LEN) : next;
}

async function fetchMetrics() {
	// 純正 backend では endpoint ごと存在しない。取得できなければ metrics を
	// null のままにして、セクション全体を出さない。
	//
	// endpoint 名の cast は misskey-js の型に存在しないため。mk-go 独自 endpoint を
	// 呼ぶ以上避けられない (job-queue.vue の admin/queue/* は upstream にもあるので
	// cast 不要という違いがある)。
	const res = await misskeyApi('admin/server-metrics' as never) as unknown as ServerMetrics;
	metrics.value = res;
	goroutineHistory.value = push(goroutineHistory.value, res.go.goroutines);
	heapHistory.value = push(heapHistory.value, res.go.heapAllocBytes);
}

onMounted(async () => {
	try {
		await fetchMetrics();
	} catch {
		return; // endpoint 不在 / 権限なし。以降ポーリングもしない。
	}
	if (disposed) return;
	timer = window.setInterval(() => {
		fetchMetrics().catch(() => {});
	}, POLL_INTERVAL_MS);
});

onUnmounted(() => {
	// ダッシュボードを離れてもポーリングが残ると、閉じ忘れたタブが 10s ごとに
	// ReadMemStats を叩き続ける。
	disposed = true;
	if (timer != null) window.clearInterval(timer);
});
</script>

<style lang="scss" module>
.root {
	&:global {
		> .status {
			padding: 0 0 16px 0;
		}

		> .charts {
			display: grid;
			grid-template-columns: 1fr 1fr;
			gap: 12px;
			padding-bottom: 16px;

			> .chart {
				min-width: 0;
				padding: 16px;
				background: var(--MI_THEME-panel);
				border-radius: var(--MI-radius);

				> .title {
					font-size: 0.85em;
				}

				> .graph {
					height: 60px;
				}
			}
		}
	}
}

.version {
	font-size: 0.85em;
	opacity: 0.7;
}
</style>
