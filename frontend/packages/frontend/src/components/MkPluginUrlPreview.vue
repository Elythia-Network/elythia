<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div ref="viewportRoot" :class="$style.root">
<MkUrlPreview
	v-if="selectedProvider == null"
	v-bind="$attrs"
	:url="url"
	:detail="detail"
	:compact="compact"
	:showActions="showActions"
/>
<component
	:is="selectedProvider.component"
	v-else-if="loadState === 'ready' && !renderFailed"
	v-bind="$attrs"
	:data="loadedData"
	:input="input"
	:context="context"
/>
<component
	:is="selectedProvider.loadingComponent"
	v-else-if="loadState === 'loading' && selectedProvider.loadingComponent != null"
	:input="input"
	:context="context"
/>
<a
	v-else-if="safeHref != null"
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
import { resolvePreviewProvider } from '@/plugin-api.js';
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
const componentActive = ref(true);
const documentVisible = ref(window.document.visibilityState === 'visible');
const viewportRoot = ref<HTMLElement | null>(null);
const inViewport = ref(typeof window.IntersectionObserver === 'undefined');
let viewportObserver: IntersectionObserver | null = null;
const active = computed(() => componentActive.value && documentVisible.value);
const context = computed<PreviewContext>(() => ({
	detail: props.detail,
	revealed: props.revealed,
	active: active.value,
	inViewport: inViewport.value,
	source: props.source,
}));
const safeHref = computed(() => {
	try {
		const parsed = new URL(props.url, window.location.href);
		return parsed.protocol === 'http:' || parsed.protocol === 'https:' ? parsed.href : null;
	} catch {
		return null;
	}
});
const mayLoad = computed(() => {
	if (registration.value == null) return false;
	if (props.source == null || !props.revealed || !active.value || !inViewport.value) return false;
	return !registration.value.provider.requiresDetail || props.detail;
});

const loadState = ref<'idle' | 'loading' | 'ready' | 'error'>('idle');
const loadedData = ref<unknown>(null);
const renderFailed = ref(false);
let controller: AbortController | null = null;
let loadGeneration = 0;
let settledKey = '';

function loadKey(): string {
	return [registration.value?.id ?? '', props.url, props.source?.noteId ?? '', props.detail ? 'detail' : 'timeline'].join('\u0000');
}

function abortLoad(): void {
	loadGeneration++;
	controller?.abort();
	controller = null;
}

function resetLoad(): void {
	abortLoad();
	loadState.value = 'idle';
	loadedData.value = null;
	settledKey = '';
	renderFailed.value = false;
}

function ownsSettledState(key: string): boolean {
	return (loadState.value === 'ready' || loadState.value === 'error') &&
		settledKey === key && props.revealed && props.source != null;
}

watch(
	() => [registration.value?.id, props.url, mayLoad.value, props.detail, props.revealed, active.value, inViewport.value, props.source?.noteId] as const,
	() => {
		const selected = registration.value;
		const nextKey = loadKey();
		if (selected == null || !mayLoad.value) {
			abortLoad();
			// Visibility and activation only gate new work. A ready result belongs
			// to this note until an ownership key changes, the CW closes, or the
			// coordinator unmounts. Keep it mounted so returning to the tab or a
			// KeepAlive view does not trigger another provider request.
			if (selected != null && ownsSettledState(nextKey)) return;
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

onActivated(() => {
	componentActive.value = true;
});

onDeactivated(() => {
	componentActive.value = false;
	abortLoad();
});

onBeforeUnmount(abortLoad);

onUnmounted(() => {
	window.document.removeEventListener('visibilitychange', updateDocumentVisibility);
	viewportObserver?.disconnect();
	viewportObserver = null;
});

onErrorCaptured((err) => {
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
</style>
