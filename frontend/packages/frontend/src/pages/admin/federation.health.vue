<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div class="_gaps">
	<!--
		純正 backend では endpoint ごと存在しない。**エラーとして出す。**
		mk-go でも telemetry 未配線なら 200 で hosts: [] が返るので、
		「繋がらない」と「データが無い」は別物として扱える。
	-->
	<MkInfo v-if="unavailable" warn>{{ i18n.ts._federationHealth.unavailable }}</MkInfo>

	<template v-else>
		<div :class="$style.summary">
			<div :class="$style.summaryItem">
				<div :class="$style.summaryLabel">{{ i18n.ts._federationHealth.successRate }}</div>
				<div :class="$style.summaryValue">{{ overallRate }}</div>
				<div :class="$style.summarySub">{{ number(overallSuccess) }} / {{ number(overallTotal) }}</div>
			</div>
			<div :class="$style.summaryItem">
				<div :class="$style.summaryLabel">{{ i18n.ts._federationHealth.hosts }}</div>
				<div :class="$style.summaryValue">{{ number(hosts.length) }}</div>
				<div :class="$style.summarySub">{{ i18n.ts._federationHealth.hostsCaption }}</div>
			</div>
			<div :class="$style.summaryItem">
				<div :class="$style.summaryLabel">{{ i18n.ts._federationHealth.degraded }}</div>
				<div :class="[$style.summaryValue, { [$style.bad]: degradedCount > 0 }]">{{ number(degradedCount) }}</div>
				<div :class="$style.summarySub">{{ i18n.ts._federationHealth.stateDegraded }}</div>
			</div>
		</div>

		<!--
			**evictedHosts が 0 でなければ、この一覧は全体の一部。** 黙って
			欠けたまま見せると「出ていない = 問題なし」と読まれる。
		-->
		<MkInfo v-if="evictedHosts > 0" warn>{{ i18n.tsx._federationHealth.evicted({ n: number(evictedHosts) }) }}</MkInfo>

		<!--
			mk-go: 配送を止めている相手 (#3048)。**「開いたまま戻らない」が最悪の
			失敗形**なので、見えて手で再開できるようにする。受信側には無い。
		-->
		<div v-if="direction === 'deliver' && breakers.length > 0" class="_gaps_s">
			<div :class="$style.breakersHead">{{ i18n.ts._federationHealth.breakersTitle }}</div>
			<div v-for="b in breakers" :key="b.host" :class="[$style.breaker, b.open ? $style.error : $style.warn]">
				<div :class="$style.breakerHead">
					<span class="_monospace">{{ b.host }}</span>
					<span :class="$style.breakerState">{{ stateLabel(b) }}</span>
				</div>
				<div :class="$style.breakerBody">
					<template v-if="b.open">
						<div v-if="b.openedAt">{{ i18n.ts._federationHealth.breakerOpenedAt }}: <MkTime :time="b.openedAt"/></div>
						<div v-if="b.nextProbeAt">{{ i18n.ts._federationHealth.breakerNextProbe }}: <MkTime :time="b.nextProbeAt" mode="absolute"/></div>
						<div>{{ i18n.tsx._federationHealth.breakerDetail({ failures: number(b.consecutiveFailures), interval: intervalLabel(b.probeIntervalSeconds) }) }}</div>
					</template>
					<div v-if="b.throttledUntil">{{ i18n.ts._federationHealth.breakerThrottledUntil }}: <MkTime :time="b.throttledUntil" mode="absolute"/></div>
					<div v-if="b.reservedUntil">{{ i18n.ts._federationHealth.breakerReservedUntil }}: <MkTime :time="b.reservedUntil" mode="absolute"/></div>
				</div>
				<MkButton small :disabled="closing === b.host" @click="closeBreaker(b.host)"><i class="ti ti-player-play"></i> {{ i18n.ts._federationHealth.breakerClose }}</MkButton>
			</div>
			<div :class="$style.hint">{{ i18n.ts._federationHealth.breakersNote }}</div>
		</div>

		<div>
			<MkInput v-model="hostQuery" :debounce="true">
				<template #prefix><i class="ti ti-search"></i></template>
				<template #label>{{ i18n.ts.host }}</template>
			</MkInput>
			<FormSplit style="margin-top: var(--MI-margin);">
				<MkSelect v-model="windowSeconds" :items="windowDef">
					<template #label>{{ i18n.ts._federationHealth.window }}</template>
				</MkSelect>
				<MkSelect v-model="sort" :items="sortDef">
					<template #label>{{ i18n.ts.sort }}</template>
				</MkSelect>
			</FormSplit>
			<FormSplit style="margin-top: var(--MI-margin);">
				<MkSelect v-model="state" :items="stateDef">
					<template #label>{{ i18n.ts.state }}</template>
				</MkSelect>
			</FormSplit>
		</div>

		<MkLoading v-if="fetching"/>
		<div v-else-if="visibleHosts.length === 0" :class="$style.empty">{{ i18n.ts._federationHealth.noHosts }}</div>
		<div v-else class="_gaps_s">
			<!--
				**key に severity を混ぜる。** defaultOpen は MkFolder のマウント時しか
				効かないので、key が host だけだと窓や並び替えを変えたときに同じ要素が
				再利用され、error に転落しても開かない。
			-->
			<MkFolder v-for="h in visibleHosts" :key="`${h.host}:${severityOf(h)}`" :defaultOpen="severityOf(h) === 'error'">
				<template #label>
					<span :class="[$style.dot, $style[severityOf(h)]]" :title="rateLabel(h)"></span>
					<span class="_monospace">{{ h.host }}</span>
				</template>
				<template #suffix>
					<span :class="[$style.rate, $style[severityOf(h)]]">{{ rateLabel(h) }}</span>
				</template>

				<div class="_gaps_s">
					<div class="_table">
						<div class="_row">
							<div class="_cell"><div class="_label">{{ i18n.ts._federationHealth.succeeded }}</div>{{ number(h.success) }}</div>
							<div class="_cell"><div class="_label">{{ i18n.ts._federationHealth.failed }}</div>{{ number(h.failure) }}</div>
							<!-- p50 / p95 は分位数の記号で、訳す対象ではないので直書き。 -->
							<div class="_cell"><div class="_label">p50</div>{{ latency(h.latencyP50Ms) }}</div>
							<div class="_cell"><div class="_label">p95</div>{{ latency(h.latencyP95Ms) }}</div>
						</div>
					</div>

					<div :class="$style.classes">
						<!-- byClass は発生した class だけを持つので値は optional。 -->
						<span v-for="(count, cls) in h.byClass" :key="cls" :class="[$style.chip, $style[toneOf(cls)]]">
							{{ cls }} <b>{{ number(count ?? 0) }}</b>
						</span>
					</div>

					<!--
						**lastError は窓に属さない。** Store は lastErrorTTL (7 日) で別に持って
						おり、Query は窓のループの外で引く。窓内の失敗が 0 でも数日前のエラーが
						出るので、そのときは期間外だと明示しないと「この期間に失敗はありません」
						と並んで矛盾して見える。
					-->
					<div v-if="h.failure === 0" :class="$style.clean">{{ i18n.ts._federationHealth.noFailures }}</div>
					<div v-else-if="!h.lastError" :class="$style.clean">{{ i18n.ts._federationHealth.noRecordedError }}</div>
					<div v-if="h.lastError" :class="[$style.lastError, $style[toneOf(h.lastError.class)], { [$style.stale]: h.failure === 0 }]">
						<div :class="$style.lastErrorHead">
							<span v-if="h.failure === 0">{{ i18n.ts._federationHealth.outsideWindow }} · </span>
							<MkTime :time="h.lastError.at"/> · {{ h.lastError.class }}<!--
								**受信側は status を出さない。** recordInboxTelemetry が Status を
								積まないので常に 0 になり、「応答なし」と出すと mk-go が応答した
								うえで弾いた事実と逆を指す。
							--><template v-if="direction === 'deliver'">
								· {{ h.lastError.status > 0 ? `HTTP ${h.lastError.status}` : i18n.ts._federationHealth.noResponse }}
							</template>
						</div>
						<!-- blocked / gone / rateLimited は message を持たない。空の pre 行を残さない。 -->
						<div v-if="h.lastError.message" :class="[$style.lastErrorMsg, '_monospace']">{{ h.lastError.message }}</div>
						<div :class="$style.hint">{{ hintFor(h.lastError.class) }}</div>
					</div>
				</div>
			</MkFolder>
		</div>

		<MkInfo>{{ direction === 'deliver' ? i18n.ts._federationHealth.deliverNote : i18n.ts._federationHealth.inboxNote }}</MkInfo>
	</template>
</div>
</template>

<script lang="ts" setup>
import { computed, ref, watch } from 'vue';
import MkInput from '@/components/MkInput.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkLoading from '@/components/global/MkLoading.vue';
import MkTime from '@/components/global/MkTime.vue';
import MkButton from '@/components/MkButton.vue';
import * as os from '@/os.js';
import FormSplit from '@/components/form/split.vue';
import { i18n } from '@/i18n.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { useMkSelect } from '@/composables/use-mkselect.js';

// **送信側と受信側で class は完全に別物。** 重なるものは 1 つも無い。
// 送信は internal/core/deliveryhealth/outcome.go の 6 種、受信は
// inbound.go の 8 種で、後者は inbox processor の分岐をそのまま写している。
type DeliverClass = 'success' | 'gone' | 'rateLimited' | 'clientError' | 'serverError' | 'transport';
type InboxClass = 'accepted' | 'unsupported' | 'signatureFailed' | 'blocked'
	| 'actorUnauthorized' | 'ldSignatureFailed' | 'processingError' | 'duplicate';
type OutcomeClass = DeliverClass | InboxClass;

// **「失敗 = 赤」ではない。** backend が success 側に数えるかどうかと、
// 運営者が対処すべきかどうかは別。unsupported / duplicate は相手が正しく
// 送っていてこちらの事情で処理しなかっただけなので緑、blocked は失敗側に
// 数えられるが**意図した拒否**なので黄にする (赤にすると自分で入れた
// ブロックが障害に見える)。
const TONE: Record<OutcomeClass, 'ok' | 'warn' | 'error'> = {
	// 送信 (deliver)
	success: 'ok',
	gone: 'warn',
	rateLimited: 'warn',
	clientError: 'error',
	serverError: 'error',
	transport: 'error',
	// 受信 (inbox)
	accepted: 'ok',
	unsupported: 'ok',
	duplicate: 'ok',
	blocked: 'warn',
	signatureFailed: 'error',
	actorUnauthorized: 'error',
	ldSignatureFailed: 'error',
	processingError: 'error',
};

type HostHealth = {
	host: string;
	success: number;
	failure: number;
	byClass: Partial<Record<OutcomeClass, number>>;
	// ヒストグラムの近似。該当バケットの上限を返し、最上位 (+Inf) は -1。
	latencyP50Ms: number;
	latencyP95Ms: number;
	lastError?: {
		at: string;
		class: OutcomeClass;
		status: number;
		message: string;
	};
};

// mk-go: 配送を止めている相手 (#3048)。internal/core/deliveryhealth/breaker.go の
// BreakerState。
type BreakerState = {
	host: string;
	// 開いている (配送を止めている) か。false なら 429 で間隔を空けているか、待たせた分を送っているだけ。
	open: boolean;
	consecutiveFailures: number;
	openedAt: string | null;
	nextProbeAt: string | null;
	probeIntervalSeconds: number;
	throttledUntil: string | null;
	// 429 で待たせた配送の最後の予約時刻 (送り終わる見込み)。
	reservedUntil: string | null;
};

type HealthResponse = {
	windowSeconds: number;
	hosts: HostHealth[];
	evictedHosts: number;
	// 古い backend (#3048 より前) と受信側には無いか空。
	breakers?: BreakerState[];
};

const props = defineProps<{
	direction: 'deliver' | 'inbox';
}>();

// 検索語はタブをまたいで引き継ぐ。同じホストを調べている最中に Deliver /
// Inbox / インスタンスを行き来するので、そのたびに打ち直させない。
const hostQuery = defineModel<string>('host', { default: '' });

const hosts = ref<HostHealth[]>([]);
const evictedHosts = ref(0);
const breakers = ref<BreakerState[]>([]);
const closing = ref<string | null>(null);
const fetching = ref(true);
const unavailable = ref(false);

const { model: windowSeconds, def: windowDef } = useMkSelect({
	// **1 時間より長い窓は出さない。** deliveryhealth.MaxWindow が 1 時間で、
	// 超える値は黙って丸められる。選ばせると指定と結果が食い違う。
	items: [
		{ label: i18n.ts._federationHealth.last5m, value: 300 },
		{ label: i18n.ts._federationHealth.last15m, value: 900 },
		{ label: i18n.ts._federationHealth.last1h, value: 3600 },
	],
	initialValue: 3600,
});

const { model: state, def: stateDef } = useMkSelect({
	items: [
		{ label: i18n.ts.all, value: 'all' },
		{ label: i18n.ts._federationHealth.stateFailing, value: 'failing' },
		{ label: i18n.ts._federationHealth.stateDegraded, value: 'degraded' },
		{ label: i18n.ts._federationHealth.stateHealthy, value: 'healthy' },
	],
	initialValue: 'all',
});

const { model: sort, def: sortDef } = useMkSelect({
	items: [
		{ label: i18n.ts._federationHealth.sortFailure, value: 'failure' },
		{ label: i18n.ts._federationHealth.sortRate, value: 'rate' },
		{ label: i18n.ts._federationHealth.sortP95, value: 'p95' },
		{ label: i18n.ts._federationHealth.sortVolume, value: 'volume' },
		{ label: i18n.ts.host, value: 'host' },
	],
	initialValue: 'failure',
});

const totalOf = (h: HostHealth) => h.success + h.failure;
const rateOf = (h: HostHealth) => (totalOf(h) === 0 ? 100 : (h.success / totalOf(h)) * 100);

function severityOf(h: HostHealth): 'ok' | 'warn' | 'error' {
	if (rateOf(h) < 95) return 'error';
	return h.failure > 0 ? 'warn' : 'ok';
}

// 未知の class は赤に倒す。backend が種別を足したときに「緑で素通り」に
// ならないようにする (見落とすより騒ぐほうが安全)。
const toneOf = (cls: OutcomeClass): 'ok' | 'warn' | 'error' => TONE[cls] ?? 'error';

// **何が起きたかではなく、次に何をすればいいかを出す。** 生の 403 だけでは
// 運営者は動けない。retry するのかしないのか、相手に連絡が要るのかが分かれ目。
function hintFor(cls: OutcomeClass): string {
	return i18n.ts._federationHealth._hints[cls];
}

const number = (n: number) => n.toLocaleString();
const latency = (ms: number) => (ms < 0 ? i18n.ts._federationHealth.overflow : `${number(ms)} ms`);
// 表記は 1 箇所で決める。summary と各行で `100.0%` / `100%` が混ざらないように。
const pct = (v: number) => `${v.toFixed(1).replace(/\.0$/, '')}%`;
const rateLabel = (h: HostHealth) => pct(rateOf(h));

const overallSuccess = computed(() => hosts.value.reduce((a, h) => a + h.success, 0));
const overallTotal = computed(() => hosts.value.reduce((a, h) => a + totalOf(h), 0));
const overallRate = computed(() => (overallTotal.value === 0 ? '—' : pct((overallSuccess.value / overallTotal.value) * 100)));
const degradedCount = computed(() => hosts.value.filter((h) => rateOf(h) < 95).length);

// **絞り込みと並び替えはクライアント側で行う。** DefaultMaxHosts が 2048 で
// 1 リクエストに収まるので、endpoint に sort / filter を足さない。
const visibleHosts = computed(() => {
	const q = hostQuery.value.trim().toLowerCase();
	const filtered = hosts.value.filter((h) => {
		if (q !== '' && !h.host.toLowerCase().includes(q)) return false;
		switch (state.value) {
			case 'failing': return h.failure > 0;
			case 'degraded': return rateOf(h) < 95;
			case 'healthy': return h.failure === 0;
			default: return true;
		}
	});

	const byHost = (a: HostHealth, b: HostHealth) => a.host.localeCompare(b.host);
	// p95 の -1 は「最上位バケットを超えた」なので、遅い順では先頭に来る。
	const p95 = (h: HostHealth) => (h.latencyP95Ms < 0 ? Number.POSITIVE_INFINITY : h.latencyP95Ms);

	return [...filtered].sort((a, b) => {
		switch (sort.value) {
			case 'rate': return rateOf(a) - rateOf(b) || byHost(a, b);
			case 'p95': return p95(b) - p95(a) || byHost(a, b);
			case 'volume': return totalOf(b) - totalOf(a) || byHost(a, b);
			case 'host': return byHost(a, b);
			// 既定はサーバー側と同じ (失敗の多い順 -> ホスト名順)。
			default: return b.failure - a.failure || byHost(a, b);
		}
	});
});

async function fetchHealth() {
	fetching.value = true;
	// **毎回戻す。** 立てたままだと v-else 側 (窓・状態・並び替えの全セレクタ) が
	// 消えるので、一度でも失敗するとその場から再試行できなくなる。
	unavailable.value = false;
	const endpoint = props.direction === 'deliver'
		? 'admin/federation/delivery-health'
		: 'admin/federation/inbox-health';
	try {
		// endpoint 名の cast は misskey-js の型に存在しないため。mk-go 独自
		// endpoint を呼ぶ以上避けられない (overview.mkgo.vue と同じ扱い)。
		const res = await misskeyApi(endpoint as never, { windowSeconds: windowSeconds.value } as never) as unknown as HealthResponse;
		hosts.value = res.hosts ?? [];
		evictedHosts.value = res.evictedHosts ?? 0;
		breakers.value = res.breakers ?? [];
		unavailable.value = false;
	} catch {
		hosts.value = [];
		evictedHosts.value = 0;
		breakers.value = [];
		unavailable.value = true;
	} finally {
		fetching.value = false;
	}
}

// 秒数を「1分」「2時間」のように出す。間隔は 1 分から 1 時間 (倍々) なので、
// 分と時間で足りる。
function intervalLabel(sec: number): string {
	if (sec >= 3600 && sec % 3600 === 0) return i18n.tsx._federationHealth.hours({ n: number(sec / 3600) });
	return i18n.tsx._federationHealth.minutes({ n: number(Math.max(1, Math.round(sec / 60))) });
}

function stateLabel(b: BreakerState): string {
	if (b.open) return i18n.ts._federationHealth.breakerOpen;
	if (b.throttledUntil) return i18n.ts._federationHealth.breakerThrottled;
	return i18n.ts._federationHealth.breakerDraining;
}

// 閉じると、待たせていたジョブは数分以内に送られる (待たせる時間は最大 5 分で
// 区切ってある)。429 の予約も消える。相手がまだ落ちていれば、また失敗を数えて止まり直すだけなので、
// 確認は軽くてよい。
async function closeBreaker(host: string) {
	const { canceled } = await os.confirm({
		type: 'question',
		text: i18n.tsx._federationHealth.breakerCloseConfirm({ host }),
	});
	if (canceled) return;
	closing.value = host;
	try {
		await os.apiWithDialog('admin/federation/close-delivery-breaker' as never, { host } as never);
		await fetchHealth();
	} finally {
		closing.value = null;
	}
}

watch([windowSeconds, () => props.direction], fetchHealth, { immediate: true });
</script>

<style lang="scss" module>
.breakersHead {
	font-weight: bold;
}

.breaker {
	display: flex;
	flex-direction: column;
	gap: 6px;
	padding: 12px 14px;
	border-radius: var(--MI-radius);
	background: var(--MI_THEME-panel);
	border-left: solid 4px var(--MI_THEME-warn);

	&.error {
		border-left-color: var(--MI_THEME-error);
	}
}

.breakerHead {
	display: flex;
	align-items: center;
	gap: 8px;
}

.breakerState {
	font-size: 0.85em;
	opacity: 0.8;
}

.breakerBody {
	font-size: 0.9em;
}

.summary {
	display: grid;
	grid-template-columns: repeat(3, 1fr);
	background: var(--MI_THEME-panel);
	border-radius: var(--MI-radius);
	overflow: hidden;
}

.summaryItem {
	padding: 16px;
	border-right: solid 1px var(--MI_THEME-divider);

	&:last-child {
		border-right: none;
	}
}

.summaryLabel {
	font-size: 0.8em;
	opacity: 0.7;
}

.summaryValue {
	font-size: 1.6em;
	line-height: 1.3;
	font-variant-numeric: tabular-nums;

	&.bad {
		color: var(--MI_THEME-error);
	}
}

.summarySub {
	font-size: 0.8em;
	opacity: 0.65;
}

.dot {
	display: inline-block;
	width: 8px;
	height: 8px;
	border-radius: 50%;
	margin-right: 8px;
	vertical-align: middle;
	background: var(--MI_THEME-success);

	&.warn { background: var(--MI_THEME-warn); }
	&.error { background: var(--MI_THEME-error); }
}

.rate {
	font-variant-numeric: tabular-nums;

	&.warn { color: var(--MI_THEME-warn); }
	&.error { color: var(--MI_THEME-error); }
}

.classes {
	display: flex;
	flex-wrap: wrap;
	gap: 6px;
}

.chip {
	border: solid 1px var(--MI_THEME-divider);
	border-radius: 999px;
	padding: 2px 10px;
	font-size: 0.85em;

	&.ok { border-color: var(--MI_THEME-success); }
	&.warn { border-color: var(--MI_THEME-warn); }
	&.error { border-color: var(--MI_THEME-error); }
}

.lastError {
	border-left: solid 3px var(--MI_THEME-error);
	background: var(--MI_THEME-bg);
	border-radius: 0 6px 6px 0;
	padding: 10px 14px;

	&.warn { border-left-color: var(--MI_THEME-warn); }

	// 窓の外のエラー。色で騒がず、記録として残っていることだけを伝える。
	&.stale {
		border-left-color: var(--MI_THEME-divider);
		opacity: 0.75;
	}
}

.lastErrorHead {
	font-size: 0.8em;
	opacity: 0.7;
}

.lastErrorMsg {
	font-size: 0.9em;
	margin-top: 2px;
	overflow-x: auto;
	white-space: pre;
}

.hint {
	font-size: 0.85em;
	margin-top: 6px;
}

.clean {
	font-size: 0.9em;
	opacity: 0.7;
}

.empty {
	text-align: center;
	padding: 32px 16px;
	opacity: 0.7;
}
</style>
