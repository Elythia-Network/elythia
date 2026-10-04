<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: 連合のルール 1 件の編集欄 (#3090)。純正 backend にはこの endpoint が無い。
	保存はルール全体の置き換えなので、欄の値をそのまま送る。
-->
<template>
<div class="_gaps_m">
	<MkInput v-model="name" :disabled="readOnly">
		<template #label>{{ i18n.ts._federationRules.name }}</template>
	</MkInput>
	<FormSplit>
		<MkSelect v-model="mode" :items="modeDef" :disabled="readOnly">
			<template #label>{{ i18n.ts._federationRules.mode }}</template>
			<template #caption>{{ i18n.ts._federationRules.modeCaption }}</template>
		</MkSelect>
		<MkSelect v-model="target" :items="targetDef" :disabled="readOnly">
			<template #label>{{ i18n.ts._federationRules.target }}</template>
		</MkSelect>
	</FormSplit>
	<div :class="$style.caption">{{ i18n.ts._federationRules.targetCaption }}</div>
	<MkInput v-model="position" type="text" inputmode="numeric" :disabled="readOnly">
		<template #label>{{ i18n.ts._federationRules.position }}</template>
		<template #caption>{{ i18n.ts._federationRules.positionCaption }}</template>
	</MkInput>

	<div :class="$style.heading">{{ i18n.ts._federationRules.conditions }}</div>
	<div :class="$style.caption">{{ i18n.ts._federationRules.conditionsCaption }}</div>
	<div v-if="target === 'activity'" class="_gaps_s">
		<div>{{ i18n.ts._federationRules.activityTypes }}</div>
		<div :class="$style.types">
			<MkSwitch v-for="t in activityTypeNames" :key="t" :modelValue="activityTypes.includes(t)" :disabled="readOnly" @update:modelValue="v => toggleType(t, v)">
				<span class="_monospace">{{ t }}</span>
			</MkSwitch>
		</div>
	</div>
	<MkTextarea v-model="hosts" :disabled="readOnly">
		<template #label>{{ i18n.ts._federationRules.hosts }}</template>
		<template #caption>{{ i18n.ts._federationRules.hostsCaption }}</template>
	</MkTextarea>
	<FormSplit>
		<MkSelect v-model="isBot" :items="triDef(i18n.ts._federationRules.bot, i18n.ts._federationRules.notBot)" :disabled="readOnly">
			<template #label>{{ i18n.ts._federationRules.isBot }}</template>
		</MkSelect>
		<MkInput v-model="newWithinHours" type="text" inputmode="numeric" :disabled="readOnly">
			<template #label>{{ i18n.ts._federationRules.newWithinHours }}</template>
		</MkInput>
	</FormSplit>
	<div :class="$style.caption">{{ i18n.ts._federationRules.newWithinHoursCaption }}</div>
	<template v-if="target === 'note'">
		<MkTextarea v-model="patterns" :disabled="readOnly">
			<template #label>{{ i18n.ts._federationRules.patterns }}</template>
			<template #caption>{{ i18n.ts._federationRules.patternsCaption }}</template>
		</MkTextarea>
		<MkSelect v-model="hasAttachment" :items="triDef(i18n.ts._federationRules.withAttachment, i18n.ts._federationRules.withoutAttachment)" :disabled="readOnly">
			<template #label>{{ i18n.ts._federationRules.hasAttachment }}</template>
		</MkSelect>
		<MkTextarea v-model="tags" :disabled="readOnly">
			<template #label>{{ i18n.ts._federationRules.tags }}</template>
			<template #caption>{{ i18n.ts._federationRules.tagsCaption }}</template>
		</MkTextarea>
	</template>

	<div :class="$style.heading">{{ i18n.ts._federationRules.actions }}</div>
	<MkSwitch v-model="reject" :disabled="readOnly">{{ i18n.ts._federationRules.reject }}</MkSwitch>
	<template v-if="target === 'note'">
		<MkSwitch v-model="stripMedia" :disabled="readOnly">{{ i18n.ts._federationRules.stripMedia }}</MkSwitch>
		<MkSwitch v-model="sensitive" :disabled="readOnly">
			{{ i18n.ts._federationRules.sensitive }}
			<template #caption>{{ i18n.ts._federationRules.sensitiveCaption }}</template>
		</MkSwitch>
		<MkSwitch v-model="unlist" :disabled="readOnly">
			{{ i18n.ts._federationRules.unlist }}
			<template #caption>{{ i18n.ts._federationRules.unlistCaption }}</template>
		</MkSwitch>
		<MkInput v-model="cw" :disabled="readOnly">
			<template #label>{{ i18n.ts._federationRules.cw }}</template>
			<template #caption>{{ i18n.ts._federationRules.cwCaption }}</template>
		</MkInput>
	</template>

	<div v-if="!readOnly" class="_buttons">
		<MkButton primary :disabled="saving" @click="save"><i class="ti ti-check"></i> {{ i18n.ts.save }}</MkButton>
		<MkButton v-if="rule.id" danger :disabled="saving" @click="del"><i class="ti ti-trash"></i> {{ i18n.ts.delete }}</MkButton>
	</div>
	<div v-if="!readOnly && rule.id" :class="$style.caption">{{ i18n.ts._federationRules.conditionsResetNote }}</div>
</div>
</template>

<script lang="ts">
// mk-go: admin/federation/rules/* の要素。misskey-js の型に無いので手で持つ。
export type FederationRule = {
	id: string | null;
	name: string;
	mode: 'disabled' | 'record' | 'enforce';
	target: 'note' | 'activity';
	position: number;
	hosts: string[];
	activityTypes: string[];
	isBot: boolean | null;
	newWithinHours: number | null;
	patterns: string[];
	hasAttachment: boolean | null;
	tags: string[];
	reject: boolean;
	stripMedia: boolean;
	sensitive: boolean;
	unlist: boolean;
	cw: string | null;
	hits?: number;
};

// backend の fedrule.ActivityTypes と揃える。
export const activityTypeNames = [
	'Follow', 'Undo', 'Accept', 'Reject', 'Create', 'Update', 'Delete',
	'Like', 'EmojiReact', 'Announce', 'Block', 'Flag', 'Move', 'Add', 'Remove',
] as const;
</script>

<script lang="ts" setup>
import { ref } from 'vue';
import MkButton from '@/components/MkButton.vue';
import MkInput from '@/components/MkInput.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkSwitch from '@/components/MkSwitch.vue';
import MkTextarea from '@/components/MkTextarea.vue';
import FormSplit from '@/components/form/split.vue';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import { useMkSelect } from '@/composables/use-mkselect.js';

const props = defineProps<{
	rule: FederationRule;
	readOnly: boolean;
}>();

const emit = defineEmits<{
	(ev: 'saved'): void;
	(ev: 'deleted'): void;
}>();

// 真偽の三択 (問わない / はい / いいえ) は MkSelect が boolean を値に取れないので
// 文字列で持つ。
type Tri = 'any' | 'yes' | 'no';

function toTri(v: boolean | null): Tri {
	return v === null ? 'any' : v ? 'yes' : 'no';
}

function fromTri(v: Tri): boolean | null {
	return v === 'any' ? null : v === 'yes';
}

function triDef(yes: string, no: string) {
	return [
		{ label: i18n.ts._federationRules.any, value: 'any' },
		{ label: yes, value: 'yes' },
		{ label: no, value: 'no' },
	];
}

function lines(v: string): string[] {
	return v.split('\n').map(x => x.trim()).filter(x => x !== '');
}

const name = ref(props.rule.name);
const { model: mode, def: modeDef } = useMkSelect({
	items: [
		{ label: i18n.ts._federationRules.modeDisabled, value: 'disabled' },
		{ label: i18n.ts._federationRules.modeRecord, value: 'record' },
		{ label: i18n.ts._federationRules.modeEnforce, value: 'enforce' },
	],
	initialValue: props.rule.mode,
});
const { model: target, def: targetDef } = useMkSelect({
	items: [
		{ label: i18n.ts._federationRules.targetNote, value: 'note' },
		{ label: i18n.ts._federationRules.targetActivity, value: 'activity' },
	],
	initialValue: props.rule.target,
});
const position = ref(String(props.rule.position));
const hosts = ref(props.rule.hosts.join('\n'));
const activityTypes = ref<string[]>([...props.rule.activityTypes]);
const isBot = ref<Tri>(toTri(props.rule.isBot));
const newWithinHours = ref(props.rule.newWithinHours === null ? '' : String(props.rule.newWithinHours));
const patterns = ref(props.rule.patterns.join('\n'));
const hasAttachment = ref<Tri>(toTri(props.rule.hasAttachment));
const tags = ref(props.rule.tags.join('\n'));
const reject = ref(props.rule.reject);
const stripMedia = ref(props.rule.stripMedia);
const sensitive = ref(props.rule.sensitive);
const unlist = ref(props.rule.unlist);
const cw = ref(props.rule.cw ?? '');
const saving = ref(false);

function toggleType(t: string, on: boolean): void {
	activityTypes.value = on ? [...activityTypes.value.filter(x => x !== t), t] : activityTypes.value.filter(x => x !== t);
}

function body() {
	const isNote = target.value === 'note';
	// activity のルールは中身の条件と書き換えを持てない (backend が弾く)。画面で
	// 隠した欄の値を送らないよう、対象に合わせて落とす。
	return {
		name: name.value,
		mode: mode.value,
		target: target.value,
		position: parseCount(position.value) ?? 0,
		hosts: lines(hosts.value),
		activityTypes: isNote ? [] : activityTypeNames.filter(t => activityTypes.value.includes(t)),
		isBot: fromTri(isBot.value),
		newWithinHours: parseCount(newWithinHours.value) ?? null,
		patterns: isNote ? lines(patterns.value) : [],
		hasAttachment: isNote ? fromTri(hasAttachment.value) : null,
		tags: isNote ? lines(tags.value) : [],
		reject: reject.value,
		stripMedia: isNote && stripMedia.value,
		sensitive: isNote && sensitive.value,
		unlist: isNote && unlist.value,
		cw: isNote && cw.value.trim() !== '' ? cw.value : null,
	};
}

// 空欄は null、整数でなければ undefined (送らずに止める)。parseInt に任せると
// "abc" が NaN になり、JSON では null = 「問わない」として送られて、書いたより
// 広いルールが黙って保存される。
function parseCount(v: string): number | null | undefined {
	const t = v.trim();
	if (t === '') return null;
	// 桁が多すぎると Number が Infinity になり、JSON では null (= 問わない) として送られる。
	const n = Number(t);
	return /^\d+$/.test(t) && Number.isSafeInteger(n) ? n : undefined;
}

async function save(): Promise<void> {
	for (const [value, field] of [[newWithinHours.value, i18n.ts._federationRules.newWithinHours], [position.value, i18n.ts._federationRules.position]] as const) {
		if (parseCount(value) === undefined) {
			os.alert({ type: 'error', text: i18n.tsx._federationRules.invalidNumber({ field }) });
			return;
		}
	}
	saving.value = true;
	try {
		// endpoint 名の cast は misskey-js の型に存在しないため (mk-go 独自)。
		if (props.rule.id) {
			await os.apiWithDialog('admin/federation/rules/update' as never, { ruleId: props.rule.id, ...body() } as never);
		} else {
			await os.apiWithDialog('admin/federation/rules/create' as never, body() as never);
		}
		emit('saved');
	} catch {
		// 失敗の理由 (検証エラーの内容) は apiWithDialog がダイアログで出している。
	} finally {
		saving.value = false;
	}
}

async function del(): Promise<void> {
	if (!props.rule.id) return;
	const { canceled } = await os.confirm({
		type: 'warning',
		text: i18n.tsx._federationRules.deleteConfirm({ name: props.rule.name || i18n.ts._federationRules.untitled }),
	});
	if (canceled) return;
	saving.value = true;
	try {
		await misskeyApi('admin/federation/rules/delete' as never, { ruleId: props.rule.id } as never);
		emit('deleted');
	} catch (err) {
		os.alert({ type: 'error', text: (err as { message?: string } | null)?.message ?? String(err) });
	} finally {
		saving.value = false;
	}
}
</script>

<style lang="scss" module>
.caption {
	font-size: 0.85em;
	opacity: 0.75;
}

.heading {
	font-weight: bold;
}

.types {
	display: grid;
	grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
	gap: 8px;
}
</style>
