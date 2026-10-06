<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div>
	<MkLoading v-if="fetching"/>
	<div v-show="!fetching" :class="$style.root">
		<canvas ref="chartEl"></canvas>
	</div>
</div>
</template>

<script lang="ts" setup>
import { onMounted, onUnmounted, useTemplateRef, ref, nextTick } from 'vue';
import { Chart } from 'chart.js';
import tinycolor from 'tinycolor2';
import { misskeyApi } from '@/utility/misskey-api.js';
import { themeManager } from '@/theme.js';
import { store } from '@/store.js';
import { useChartTooltip } from '@/composables/use-chart-tooltip.js';
import { chartVLine } from '@/utility/chart-vline.js';
import { initChart } from '@/utility/init-chart.js';

initChart();

const chartEl = useTemplateRef('chartEl');
const now = new Date();
let chartInstance: Chart | null = null;
const chartLimit = 30;
const fetching = ref(true);

const { handler: externalTooltipHandler } = useChartTooltip();

async function renderChart() {
	if (chartInstance) {
		chartInstance.destroy();
	}

	const getDate = (ago: number) => {
		const y = now.getFullYear();
		const m = now.getMonth();
		const d = now.getDate();

		return new Date(y, m, d - ago);
	};

	const format = (arr: number[]) => {
		return arr.map((v, i) => ({
			x: getDate(i).getTime(),
			y: v,
		}));
	};

	const raw = await misskeyApi('charts/active-users', { limit: chartLimit, span: 'day' });

	fetching.value = false;

	await nextTick();

	if (chartEl.value == null) return;

	// Elythia: エントランスはテーマの変数を部分木で上書きして夜空にしている
	// (utility/elythia-entrance.ts)。Chart.js は CSS を読まず、既定の色は利用者のテーマから
	// 取るので、canvas の位置で解決した色を渡す。上書きの無い場所では同じ値になる
	const style = getComputedStyle(chartEl.value);
	const onNight = style.getPropertyValue('--ELYTHIA-panelBorder').trim() !== '';
	const dark = store.s.darkMode || onNight;
	const fg = style.getPropertyValue('--MI_THEME-fg').trim() || themeManager.currentCompiledTheme!.fg;
	const gridColor = dark ? 'rgba(255, 255, 255, 0.1)' : 'rgba(0, 0, 0, 0.1)';
	const vLineColor = dark ? 'rgba(255, 255, 255, 0.2)' : 'rgba(0, 0, 0, 0.2)';

	const accent = tinycolor(style.getPropertyValue('--MI_THEME-accent').trim() || themeManager.currentCompiledTheme!.accent).toHexString();

	const colorRead = accent;
	const colorWrite = '#2ecc71';

	const max = Math.max(...raw.read);

	chartInstance = new Chart(chartEl.value, {
		type: 'bar',
		data: {
			datasets: [{
				parsing: false,
				label: 'Read',
				data: format(raw.read).slice().reverse(),
				pointRadius: 0,
				borderWidth: 0,
				borderJoinStyle: 'round',
				borderRadius: 4,
				backgroundColor: colorRead,
				barPercentage: 0.5,
				categoryPercentage: 1,
				fill: true,
			}],
		},
		options: {
			aspectRatio: 2.5,
			layout: {
				padding: {
					left: 0,
					right: 8,
					top: 0,
					bottom: 0,
				},
			},
			scales: {
				x: {
					type: 'time',
					offset: true,
					time: {
						unit: 'day',
						displayFormats: {
							day: 'M/d',
							month: 'Y/M',
						},
					},
					grid: {
						display: false,
					},
					ticks: {
						stepSize: 1,
						display: true,
						maxRotation: 0,
						autoSkipPadding: 8,
						color: fg,
					},
				},
				y: {
					position: 'left',
					suggestedMax: 10,
					grid: {
						display: true,
						color: gridColor,
					},
					ticks: {
						display: true,
						//mirror: true,
						color: fg,
					},
				},
			},
			interaction: {
				intersect: false,
				mode: 'index',
			},
			plugins: {
				legend: {
					display: false,
				},
				tooltip: {
					enabled: false,
					mode: 'index',
					animation: {
						duration: 0,
					},
					external: externalTooltipHandler,
				},
			},
		},
		plugins: [chartVLine(vLineColor)],
	});
}

onMounted(() => {
	renderChart();
});

onUnmounted(() => {
	chartInstance?.destroy();
});
</script>

<style lang="scss" module>
.root {
	padding: 20px;
}
</style>
