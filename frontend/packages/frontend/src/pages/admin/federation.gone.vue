<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: 消えたサーバーとのフォロー関係の片付け (#3067)。純正 backend には
	この endpoint が無い。**自動では消さない** — 取り返しがつかないので、候補を
	並べて管理者が実行する。
-->
<template>
<div class="_gaps_m">
	<div :class="$style.caption">{{ i18n.ts._goneInstances.description }}</div>
	<MkInfo v-if="unavailable" warn>{{ i18n.ts._goneInstances.unavailable }}</MkInfo>
	<MkLoading v-else-if="loading"/>
	<div v-else-if="items.length === 0" :class="$style.caption">{{ i18n.ts._goneInstances.none }}</div>
	<template v-else>
		<div v-for="g in items" :key="g.host" :class="$style.item">
			<MkA class="_monospace" :class="$style.host" :to="`/instance-info/${g.host}`">{{ g.host }}</MkA>
			<div :class="$style.body">
				<div>{{ i18n.ts._goneInstances.goneSince }}: <MkTime v-if="g.suspendedAt" :time="g.suspendedAt" mode="detail"/><span v-else>{{ i18n.ts._goneInstances.unknown }}</span></div>
				<div>{{ i18n.tsx._goneInstances.relations({ followers: number(g.followers), following: number(g.following), requests: number(g.followRequests) }) }}</div>
			</div>
			<div>
				<MkButton danger small :disabled="cleaning !== null || total(g) === 0" @click="clean(g)"><i class="ti ti-unlink"></i> {{ i18n.ts._goneInstances.clean }}</MkButton>
			</div>
		</div>
	</template>
</div>
</template>

<script lang="ts" setup>
import { ref } from 'vue';
import type * as Elythia from 'elythia-js';
import MkButton from '@/components/MkButton.vue';
import MkInfo from '@/components/MkInfo.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import number from '@/filters/number.js';

// mk-go: admin/federation/gone-instances の要素。suspendedAt は、TS が立てた停止など
// 記録が無いものでは null。
type GoneInstance = Elythia.GoneInstance;

const items = ref<GoneInstance[]>([]);
const loading = ref(true);
const unavailable = ref(false);
const cleaning = ref<string | null>(null);

function total(g: GoneInstance): number {
	return g.followers + g.following + g.followRequests;
}

async function fetchList(): Promise<void> {
	try {
		const res = await misskeyApi('admin/federation/gone-instances', {});
		items.value = res;
		unavailable.value = false;
	} catch {
		unavailable.value = true;
	} finally {
		loading.value = false;
	}
}

async function clean(g: GoneInstance): Promise<void> {
	const { canceled } = await os.confirm({
		type: 'warning',
		text: i18n.tsx._goneInstances.cleanConfirm({
			host: g.host,
			followers: number(g.followers),
			following: number(g.following),
			requests: number(g.followRequests),
		}),
	});
	if (canceled) return;
	cleaning.value = g.host;
	try {
		const res = await misskeyApi('admin/federation/clean-gone-instance', { host: g.host });
		let text = i18n.tsx._goneInstances.cleaned({
			followers: number(res.removedFollowers),
			following: number(res.removedFollowing),
			requests: number(res.removedFollowRequests),
		});
		if (res.remaining > 0) text += '\n' + i18n.tsx._goneInstances.remaining({ n: number(res.remaining) });
		os.alert({ type: 'success', text });
	} catch (err) {
		// 一覧を開いてから戻された (もう消えたサーバーではない) ときと、別の操作で
		// 片付け中のときは、理由を伝えて読み直す。
		const code = (err as { code?: string } | null)?.code;
		os.alert({
			type: 'error',
			text: code === 'INSTANCE_NOT_GONE' ? i18n.ts._goneInstances.notGone
			: code === 'CLEANUP_IN_PROGRESS' ? i18n.ts._goneInstances.inProgress
			: (err as { message?: string } | null)?.message ?? String(err),
		});
	} finally {
		cleaning.value = null;
		await fetchList();
	}
}

fetchList();
</script>

<style lang="scss" module>
.caption {
	font-size: 0.9em;
	opacity: 0.8;
}

.item {
	display: flex;
	flex-direction: column;
	gap: 6px;
	padding: 12px 14px;
	border-radius: var(--MI-radius);
	background: var(--MI_THEME-panel);
}

.host {
	font-weight: bold;
	word-break: break-all;
}

.body {
	font-size: 0.9em;
}
</style>
