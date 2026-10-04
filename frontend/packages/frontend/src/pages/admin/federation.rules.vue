<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: 連合のルール (#3090)。純正 backend にはこの endpoint が無い。
	変更は管理者だけができる (backend が弾く) ので、モデレーターには読むだけの
	欄を出す。
-->
<template>
<div class="_gaps_m">
	<div :class="$style.caption">{{ i18n.ts._federationRules.description }}</div>
	<MkInfo v-if="unavailable" warn>{{ i18n.ts._federationRules.unavailable }}</MkInfo>
	<MkLoading v-else-if="loading"/>
	<template v-else>
		<MkInfo v-if="!iAmAdmin">{{ i18n.ts._federationRules.readOnly }}</MkInfo>
		<div v-else-if="draft === null">
			<MkButton primary @click="addDraft"><i class="ti ti-plus"></i> {{ i18n.ts._federationRules.add }}</MkButton>
		</div>
		<MkFolder v-if="draft !== null" :defaultOpen="true">
			<template #label>{{ i18n.ts._federationRules.add }}</template>
			<XRuleEditor :rule="draft" :readOnly="false" @saved="onSaved" @deleted="onSaved"/>
		</MkFolder>
		<div v-if="items.length === 0 && draft === null" :class="$style.caption">{{ i18n.ts._federationRules.none }}</div>
		<MkFolder v-for="r in items" :key="r.id ?? ''">
			<template #label>{{ r.name || i18n.ts._federationRules.untitled }}</template>
			<template #caption>{{ modeLabel(r) }} · {{ i18n.tsx._federationRules.hits({ n: number(r.hits ?? 0) }) }}</template>
			<template #suffix><span :class="[$style.mode, $style[r.mode]]">{{ modeLabel(r) }}</span></template>
			<div class="_gaps_m">
				<XRuleEditor :key="r.id + ':' + version" :rule="r" :readOnly="!iAmAdmin" @saved="fetchList" @deleted="fetchList"/>
				<div class="_gaps_s">
					<MkButton small @click="loadHits(r)"><i class="ti ti-list-search"></i> {{ i18n.ts._federationRules.showHits }}</MkButton>
					<template v-if="r.id !== null && hits[r.id] !== undefined">
						<div v-if="hits[r.id].length === 0" :class="$style.caption">{{ i18n.ts._federationRules.noHits }}</div>
						<div v-for="(h, i) in hits[r.id]" :key="i" :class="$style.hit">
							<MkTime :time="h.at" mode="detail"/>
							<span :class="h.applied ? $style.applied : $style.recorded">{{ h.applied ? i18n.ts._federationRules.applied : i18n.ts._federationRules.recordedOnly }}</span>
							<span class="_monospace">{{ h.kind }}</span>
							<MkA class="_monospace" :to="`/instance-info/${h.host}`">{{ h.host }}</MkA>
							<a v-if="isHttp(h.subject)" class="_link _monospace" :class="$style.subject" :href="h.subject" target="_blank" rel="noopener noreferrer nofollow">{{ h.subject }}</a>
							<span v-else class="_monospace" :class="$style.subject">{{ h.subject }}</span>
						</div>
					</template>
				</div>
			</div>
		</MkFolder>
	</template>
</div>
</template>

<script lang="ts" setup>
import { ref } from 'vue';
import MkButton from '@/components/MkButton.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import XRuleEditor from '@/pages/admin/federation.rule-editor.vue';
import type { FederationRule } from '@/pages/admin/federation.rule-editor.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import { iAmAdmin } from '@/i.js';
import number from '@/filters/number.js';

// mk-go: admin/federation/rules/hits の要素。
type Hit = {
	ruleId: string;
	host: string;
	subject: string;
	kind: string;
	applied: boolean;
	at: string;
};

const items = ref<FederationRule[]>([]);
const loading = ref(true);
const unavailable = ref(false);
const draft = ref<FederationRule | null>(null);
const hits = ref<Record<string, Hit[]>>({});
// 保存し直したら編集欄を作り直す (欄は開いたときの値を持つので、読み直した値を
// 反映させるには key を変える)。
const version = ref(0);

function modeLabel(r: FederationRule): string {
	return r.mode === 'enforce' ? i18n.ts._federationRules.modeEnforce
		: r.mode === 'record' ? i18n.ts._federationRules.modeRecord
		: i18n.ts._federationRules.modeDisabled;
}

function isHttp(s: string): boolean {
	return s.startsWith('https://') || s.startsWith('http://');
}

async function fetchList(): Promise<void> {
	try {
		// endpoint 名の cast は misskey-js の型に存在しないため (mk-go 独自)。
		items.value = await misskeyApi('admin/federation/rules/list' as never, {} as never) as unknown as FederationRule[];
		unavailable.value = false;
		hits.value = {};
		version.value++;
	} catch {
		unavailable.value = true;
	} finally {
		loading.value = false;
	}
}

function addDraft(): void {
	// 新しいルールは「記録だけ」で始める。いきなり効かせると誤爆に気付けない。
	draft.value = {
		id: null, name: '', mode: 'record', target: 'note',
		position: items.value.reduce((max, r) => Math.max(max, r.position + 1), 0),
		hosts: [], activityTypes: [], isBot: null, newWithinHours: null, patterns: [],
		hasAttachment: null, tags: [], reject: true, stripMedia: false, sensitive: false, unlist: false, cw: null,
	};
}

async function onSaved(): Promise<void> {
	draft.value = null;
	await fetchList();
}

async function loadHits(r: FederationRule): Promise<void> {
	if (r.id === null) return;
	const id = r.id;
	try {
		const res = await misskeyApi('admin/federation/rules/hits' as never, { ruleId: id } as never) as unknown as Hit[];
		hits.value = { ...hits.value, [id]: res };
	} catch (err) {
		os.alert({ type: 'error', text: (err as { message?: string } | null)?.message ?? String(err) });
	}
}

fetchList();
</script>

<style lang="scss" module>
.caption {
	font-size: 0.9em;
	opacity: 0.8;
}

.mode {
	font-size: 0.85em;
}

.enforce {
	color: var(--MI_THEME-warn);
}

.record {
	color: var(--MI_THEME-accent);
}

.disabled {
	opacity: 0.6;
}

.hit {
	display: flex;
	flex-wrap: wrap;
	gap: 8px;
	font-size: 0.85em;
}

.applied {
	color: var(--MI_THEME-warn);
}

.recorded {
	opacity: 0.7;
}

.subject {
	word-break: break-all;
}
</style>
