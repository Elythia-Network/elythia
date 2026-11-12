<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div ref="viewportRoot" :class="$style.root">
	<MkUrlPreview
		v-if="selectedProvider == null || showNativeFallback"
		v-bind="$attrs"
		:url="url"
		:detail="detail"
		:compact="compact"
		:showActions="showActions && !showManualAction"
	/>
	<button
		v-if="showManualAction"
		type="button"
		class="_button"
		:class="$style.manualButton"
		@click="activateManualPreview"
	>
		{{ selectedProvider?.manualLabel ?? i18n.ts.show }}
	</button>
	<button
		v-else-if="showRetryAction"
		type="button"
		class="_button"
		:class="$style.manualButton"
		@click="retryProviderLoad"
	>
		{{ i18n.ts.retry }}
	</button>
	<component
		:is="selectedProvider.component"
		v-else-if="selectedProvider != null && loadState === 'ready' && !renderFailed"
		v-bind="$attrs"
		:data="loadedData"
		:input="input"
		:context="context"
	/>
	<component
		:is="selectedProvider.loadingComponent"
		v-else-if="selectedProvider != null && loadState === 'loading' && selectedProvider.loadingComponent != null"
		:input="input"
		:context="context"
	/>
	<a
		v-else-if="selectedProvider != null && !showNativeFallback && safeHref != null"
		v-bind="$attrs"
		:class="[$attrs.class, $style.sourceLink]"
		:href="safeHref"
		target="_blank"
		rel="nofollow noopener noreferrer"
	>{{ url }}</a>
</div>
</template>

<script lang="ts" setup>
import { computed, onActivated, onBeforeUnmount, onDeactivated, onErrorCaptured, onMounted, onUnmounted, ref, watch } from 'vue';
import MkUrlPreview from '@/components/MkUrlPreview.vue';
import { i18n } from '@/i18n.js';
import { resolvePreviewProvider } from '@/plugin-api.js';
import { prefer } from '@/preferences.js';
import type { PreviewContext, PreviewInput } from '@/plugin-api.js';

defineOptions({ inheritAttrs: false });

const props = withDefaults(defineProps<{
	url: string;
	detail?: boolean;
	compact?: boolean;
	showActions?: boolean;
	revealed?: boolean;
	source?: PreviewContext['source'];
}>(), {
	detail: false,
	compact: false,
	showActions: true,
	revealed: true,
	source: null,
});

const input = computed<PreviewInput>(() => ({ kind: 'url', href: props.url }));
const registration = computed(() => resolvePreviewProvider(input.value));
const selectedProvider = computed(() => registration.value?.provider ?? null);
const preferenceValue = computed<unknown>(() => {
	const key = selectedProvider.value?.preferenceKey;
	if (key == null) return undefined;
	const reactive = (prefer.r as Record<string, { value: unknown } | undefined>)[key];
	return reactive?.value ?? (prefer.s as Readonly<Record<string, unknown>>)[key];
});
const componentActive = ref(true);
const documentVisible = ref(window.document.visibilityState === 'visible');
const viewportRoot = ref<HTMLElement | null>(null);
const inViewport = ref(typeof window.IntersectionObserver === 'undefined');
const manualActivated = ref(false);
let viewportObserver: IntersectionObserver | null = null;
const active = computed(() => componentActive.value && documentVisible.value);
const safeHref = computed(() => {
	try {
		const parsed = new URL(props.url, window.location.href);
		return parsed.protocol === 'http:' || parsed.protocol === 'https:' ? parsed.href : null;
	} catch {
		return null;
	}
});
const context = computed<PreviewContext>(() => ({
	detail: props.detail,
	manual: manualActivated.value,
	preferenceValue: preferenceValue.value,
	revealed: props.revealed,
	active: active.value,
	inViewport: inViewport.value,
	source: props.source,
}));
const presentation = computed(() => {
	const provider = selectedProvider.value;
	if (provider?.presentation == null) return 'automatic';
	try {
		const value = provider.presentation(input.value, context.value);
		return value === 'manual' || value === 'disabled' || value === 'automatic' ? value : 'disabled';
	} catch (err) {
		console.warn(`[preview:${registration.value?.id}] presentation failed`, err);
		return 'disabled';
	}
});
const canPresent = computed(() => props.source != null && props.revealed && active.value && inViewport.value && safeHref.value != null &&
	(selectedProvider.value?.requiresDetail !== true || props.detail));
const showManualAction = computed(() => selectedProvider.value != null && presentation.value === 'manual' && canPresent.value && !manualActivated.value && props.showActions);
const showNativeFallback = computed(() => selectedProvider.value != null && safeHref.value != null && (
	(presentation.value === 'manual' && canPresent.value && !manualActivated.value) ||
	(loadState.value === 'error' && selectedProvider.value.presentation != null && canPresent.value)
));
const showRetryAction = computed(() => selectedProvider.value != null && loadState.value === 'error' && selectedProvider.value.presentation != null && props.showActions &&
	(presentation.value !== 'disabled') && canPresent.value);
const mayLoad = computed(() => selectedProvider.value != null && canPresent.value && presentation.value !== 'disabled' &&
	(presentation.value !== 'manual' || manualActivated.value));

const loadState = ref<'idle' | 'loading' | 'ready' | 'error'>('idle');
const loadedData = ref<unknown>(null);
const renderFailed = ref(false);
const providerRetryAttempt = ref(0);
let controller: AbortController | null = null;
let loadGeneration = 0;
type PreviewLoadKey = readonly unknown[];
let settledKey: PreviewLoadKey | null = null;

function loadKey(): PreviewLoadKey {
	return [registration.value?.id ?? '', props.url, props.source?.noteId ?? '', props.detail, preferenceValue.value, presentation.value, manualActivated.value, providerRetryAttempt.value];
}

function sameLoadKey(left: PreviewLoadKey | null, right: PreviewLoadKey): boolean {
	return left != null && left.length === right.length && left.every((value, index) => Object.is(value, right[index]));
}

function activateManualPreview(): void {
	if (!showManualAction.value) return;
	manualActivated.value = true;
}

function retryProviderLoad(): void {
	if (selectedProvider.value == null || loadState.value === 'loading' || !props.showActions || !canPresent.value) return;
	providerRetryAttempt.value++;
}

watch(
	() => [registration.value?.id, props.url, props.source?.noteId, preferenceValue.value, props.detail, props.revealed] as const,
	(current, previous) => {
		if (previous != null && current.some((value, index) => value !== previous[index])) manualActivated.value = false;
	},
);

function abortLoad(): void {
	loadGeneration++;
	controller?.abort();
	controller = null;
}

function resetLoad(): void {
	abortLoad();
	loadState.value = 'idle';
	loadedData.value = null;
	settledKey = null;
	renderFailed.value = false;
}

function ownsSettledState(key: PreviewLoadKey): boolean {
	return (loadState.value === 'ready' || loadState.value === 'error') && sameLoadKey(settledKey, key) && props.revealed && props.source != null;
}

watch(
	() => [registration.value?.id, props.url, mayLoad.value, props.detail, props.revealed, active.value, inViewport.value, props.source?.noteId, preferenceValue.value, presentation.value, manualActivated.value, providerRetryAttempt.value] as const,
	() => {
		const selected = registration.value;
		const nextKey = loadKey();
		if (selected == null || !mayLoad.value) {
			abortLoad();
			// Visibility only gates new work. A settled result stays owned by the same
			// note until its URL, preference, CW, session source, or owner changes.
			if (selected != null && presentation.value !== 'disabled' && ownsSettledState(nextKey)) return;
			resetLoad();
			return;
		}
		if (ownsSettledState(nextKey)) return;
		resetLoad();

		const currentGeneration = loadGeneration;
		const currentController = new AbortController();
		controller = currentController;
		loadState.value = 'loading';

		selected.provider.load(input.value, context.value, currentController.signal).then(data => {
			if (currentController.signal.aborted || currentGeneration !== loadGeneration) return;
			controller = null;
			loadedData.value = data;
			settledKey = nextKey;
			loadState.value = 'ready';
		}, err => {
			if (currentController.signal.aborted || currentGeneration !== loadGeneration) return;
			controller = null;
			settledKey = nextKey;
			loadState.value = 'error';
			console.warn(`[preview:${selected.id}] load failed`, err);
		});
	},
	{ immediate: true },
);

function updateDocumentVisibility(): void {
	documentVisible.value = window.document.visibilityState === 'visible';
}

onMounted(() => {
	window.document.addEventListener('visibilitychange', updateDocumentVisibility);
	if (typeof window.IntersectionObserver !== 'undefined' && viewportRoot.value != null) {
		viewportObserver = new window.IntersectionObserver(entries => {
			inViewport.value = entries.some(entry => entry.isIntersecting);
		});
		viewportObserver.observe(viewportRoot.value);
	}
});

onActivated(() => { componentActive.value = true; });
onDeactivated(() => { componentActive.value = false; abortLoad(); });
onBeforeUnmount(abortLoad);
onUnmounted(() => {
	window.document.removeEventListener('visibilitychange', updateDocumentVisibility);
	viewportObserver?.disconnect();
	viewportObserver = null;
});

onErrorCaptured(err => {
	if (registration.value == null || (loadState.value !== 'loading' && loadState.value !== 'ready')) return;
	console.error(`[preview:${registration.value.id}] render failed`, err);
	abortLoad();
	settledKey = loadKey();
	loadState.value = 'error';
	renderFailed.value = true;
	return false;
});
</script>

<style lang="scss" module>
.root { min-width: 0; }
.sourceLink {
	display: block;
	min-width: 0;
	padding: 12px;
	border: solid 1px var(--MI_THEME-divider);
	border-radius: var(--MI-radius);
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}
.manualButton {
	display: block;
	width: 100%;
	margin-top: 8px;
	padding: 10px 14px;
	border: solid 1px var(--MI_THEME-divider);
	border-radius: var(--MI-radius);
	color: var(--MI_THEME-accent);
	background: var(--MI_THEME-panel);
}
</style>
