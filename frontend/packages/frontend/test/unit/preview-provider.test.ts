/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/vue';
import { defineComponent, nextTick, onBeforeUnmount, onMounted, ref } from 'vue';
import { components } from '@/components/index.js';
import { directives } from '@/directives/index.js';
import MkPluginUrlPreview from '@/components/MkPluginUrlPreview.vue';
import {
	_resetPreviewsForTest,
	launchServerPlugins,
	resolvePreviewProvider,
	type PluginDefinition,
	type PreviewInput,
	type PreviewProvider,
} from '@/plugin-api.js';

const StubPreview = defineComponent({
	name: 'StubPreview',
	template: '<div>stub preview</div>',
});

function provider(id: string, priority = 0, match: PreviewProvider['match'] = () => true): PreviewProvider {
	return {
		id,
		priority,
		match,
		async load() {
			return null;
		},
		component: StubPreview,
	};
}

function plugin(name: string, ...providers: PreviewProvider[]): PluginDefinition {
	return {
		name,
		setup(host) {
			for (const previewProvider of providers) host.preview.register(previewProvider);
		},
	};
}

const urlInput = { kind: 'url', href: 'https://example.com/post/1' } satisfies PreviewInput;

beforeEach(() => {
	vi.stubGlobal('IntersectionObserver', class {
		readonly root = null;
		readonly rootMargin = '0px';
		readonly thresholds = [0];
		constructor(private callback: IntersectionObserverCallback) {}
		disconnect() {}
		observe(target: Element) { this.callback([{ isIntersecting: true, target } as IntersectionObserverEntry], this as unknown as IntersectionObserver); }
		takeRecords(): IntersectionObserverEntry[] { return []; }
		unobserve() {}
	});
});

afterEach(() => {
	_resetPreviewsForTest();
	fetchMock.resetMocks();
	cleanup();
	Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

describe('preview provider registry', () => {
	test('forwards an AbortSignal through the source-compatible host API', async () => {
		fetchMock.mockOnceIf(request => new URL(request.url).pathname === '/api/plugin/example/test', () => ({ status: 204 }));
		const controller = new AbortController();
		await launchServerPlugins([{
			name: 'example',
			async setup(host) {
				await host.api('plugin/example/test', {}, { signal: controller.signal });
			},
		}]);

		const calls = fetchMock.mock.calls.filter(call => {
			const value = call[0] as string | { url?: string };
			return new URL(typeof value === 'string' ? value : value.url ?? String(value), window.location.href).pathname === '/api/plugin/example/test';
		});
		expect(calls).toHaveLength(1);
		expect(calls[0][1]?.signal).toBe(controller.signal);
	});

	test('rejects duplicate namespaced IDs', async () => {
		let duplicateError: unknown;
		await launchServerPlugins([{
			name: 'example',
			setup(host) {
				host.preview.register(provider('card'));
				try {
					host.preview.register(provider('card'));
				} catch (err) {
					duplicateError = err;
				}
			},
		}]);

		expect(duplicateError).toBeInstanceOf(Error);
		expect((duplicateError as Error).message).toContain('example:card');
	});

	test('selects priority descending, then namespaced ID ascending', async () => {
		vi.spyOn(String.prototype, 'localeCompare').mockReturnValue(1);
		await launchServerPlugins([
			plugin('zeta', provider('card', 10)),
			plugin('alpha', provider('later', 10)),
			plugin('beta', provider('winner', 20)),
		]);

		expect(resolvePreviewProvider(urlInput)?.id).toBe('beta:winner');

		_resetPreviewsForTest();
		await launchServerPlugins([
			plugin('zeta', provider('card', 10)),
			plugin('alpha', provider('later', 10)),
		]);

		expect(resolvePreviewProvider(urlInput)?.id).toBe('alpha:later');
	});

	test('skips a matcher that throws and warns when multiple providers match', async () => {
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
		await launchServerPlugins([
			plugin('broken', provider('throws', 100, () => {
				throw new Error('matcher failed');
			})),
			plugin('second', provider('card', 5)),
			plugin('first', provider('card', 10)),
		]);

		expect(resolvePreviewProvider(urlInput)?.id).toBe('first:card');
		expect(warn).toHaveBeenCalledWith(expect.stringContaining('broken:throws'), expect.any(Error));
		expect(warn).toHaveBeenCalledWith(expect.stringContaining('first:card'));
		expect(warn).toHaveBeenCalledWith(expect.stringContaining('second:card'));
	});

	test('passes URL and file inputs to pure synchronous matchers unchanged', async () => {
		const seen: PreviewInput[] = [];
		await launchServerPlugins([
			plugin('inputs', provider('all', 0, input => {
				seen.push(input);
				return true;
			})),
		]);

		const fileInput: PreviewInput = {
			kind: 'file',
			file: {
				id: 'file-1',
				name: 'clip.bin',
				size: 42,
				url: 'https://files.example/clip.bin',
				declaredMimeType: 'application/octet-stream',
				detectedMimeType: 'video/webm',
			},
		};

		resolvePreviewProvider(urlInput);
		resolvePreviewProvider(fileInput);

		expect(seen).toEqual([urlInput, fileInput]);
		expect((seen[1] as Extract<PreviewInput, { kind: 'file' }>).file).toMatchObject({
			declaredMimeType: 'application/octet-stream',
			detectedMimeType: 'video/webm',
		});
	});
});

describe('preview provider coordinator', () => {
	const source = { kind: 'note' as const, noteId: 'note-1' };

	function renderPreview(props: Record<string, unknown> = {}) {
		fetchMock.mockOnceIf(request => new URL(request.url).pathname === '/url', () => ({
			status: 200,
			body: JSON.stringify({
				url: urlInput.href,
				player: { url: null, width: null, height: null, allow: [] },
			}),
		}));
		return render(MkPluginUrlPreview, {
			props: {
				url: urlInput.href,
				detail: true,
				revealed: true,
				source,
				...props,
			},
			global: { directives, components },
		});
	}

	function lazyProvider(overrides: Partial<PreviewProvider> = {}): PreviewProvider {
		return {
			...provider('card'),
			requiresDetail: true,
			component: defineComponent({
				props: ['data'],
				template: '<div>provider: {{ data }}</div>',
			}),
			async load() {
				return 'loaded';
			},
			...overrides,
		};
	}

	function controlIntersection(): (isIntersecting: boolean) => void {
		let callback: IntersectionObserverCallback | undefined;
		vi.stubGlobal('IntersectionObserver', class {
			readonly root = null;
			readonly rootMargin = '0px';
			readonly thresholds = [0];
			constructor(value: IntersectionObserverCallback) { callback = value; }
			disconnect() {}
			observe() {}
			takeRecords(): IntersectionObserverEntry[] { return []; }
			unobserve() {}
		});
		return isIntersecting => callback?.([{ isIntersecting } as IntersectionObserverEntry], {} as IntersectionObserver);
	}

	test.each([
		['timeline', { detail: false }],
		['hidden CW', { revealed: false }],
		['missing source', { source: null }],
	])('does not load or mount the legacy preview when matched in %s context', async (_name, props) => {
		const load = vi.fn(async () => 'loaded');
		await launchServerPlugins([plugin('example', lazyProvider({ load }))]);

		const result = renderPreview(props);
		await nextTick();

		expect(load).not.toHaveBeenCalled();
		expect(fetchMock).not.toHaveBeenCalled();
		expect(result.container.querySelector('iframe')).toBeNull();
		expect(result.getByRole('link').getAttribute('href')).toBe(urlInput.href);
	});

	test('does not load while the document is inactive', async () => {
		Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
		const load = vi.fn(async () => 'loaded');
		await launchServerPlugins([plugin('example', lazyProvider({ load }))]);

		const result = renderPreview();
		await nextTick();

		expect(load).not.toHaveBeenCalled();
		expect(fetchMock).not.toHaveBeenCalled();
		expect(result.getByRole('link').getAttribute('href')).toBe(urlInput.href);
	});

	test('suppresses legacy URL loading for a gated matched service URL', async () => {
		const serviceUrl = 'https://preview.example/post/1234567890';
		const load = vi.fn(async () => 'loaded');
		await launchServerPlugins([plugin('example', lazyProvider({ load }))]);

		const result = renderPreview({ url: serviceUrl, revealed: false });
		await nextTick();

		expect(load).not.toHaveBeenCalled();
		expect(fetchMock).not.toHaveBeenCalled();
		expect(result.container.querySelector('iframe')).toBeNull();
		expect(result.getByRole('link').getAttribute('href')).toBe(serviceUrl);
	});

	test('mounts the legacy preview only when no provider matches', async () => {
		await launchServerPlugins([plugin('example', lazyProvider({ match: () => false }))]);

		renderPreview();

		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
		expect(new URL(fetchMock.mock.calls[0][0].toString(), window.location.href).pathname).toBe('/url');
	});

	test('loads and renders the selected provider only after every gate passes', async () => {
		const load = vi.fn(async (_input, context) => `${context.source?.noteId}:loaded`);
		await launchServerPlugins([plugin('example', lazyProvider({ load }))]);

		const result = renderPreview();

		await waitFor(() => expect(result.getByText('provider: note-1:loaded')).toBeTruthy());
		expect(load).toHaveBeenCalledTimes(1);
		expect(fetchMock).not.toHaveBeenCalled();
	});

	test('loads a timeline provider only after its card enters the viewport', async () => {
		let intersectionCallback: IntersectionObserverCallback | undefined;
		vi.stubGlobal('IntersectionObserver', class {
			readonly root = null;
			readonly rootMargin = '0px';
			readonly thresholds = [0];
			constructor(callback: IntersectionObserverCallback) { intersectionCallback = callback; }
			disconnect() {}
			observe() {}
			takeRecords(): IntersectionObserverEntry[] { return []; }
			unobserve() {}
		});
		const load = vi.fn(async (_input, value) => `viewport:${value.inViewport}`);
		await launchServerPlugins([plugin('example', lazyProvider({ requiresDetail: false, load }))]);

		const result = renderPreview({ detail: false });
		await nextTick();
		expect(load).not.toHaveBeenCalled();
		expect(result.getByRole('link').getAttribute('href')).toBe(urlInput.href);

		intersectionCallback?.([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver);
		await waitFor(() => expect(result.getByText('provider: viewport:true')).toBeTruthy());
		expect(load).toHaveBeenCalledTimes(1);
	});

	test('keeps a ready provider mounted across viewport exit and re-entry', async () => {
		let intersectionCallback: IntersectionObserverCallback | undefined;
		vi.stubGlobal('IntersectionObserver', class {
			readonly root = null;
			readonly rootMargin = '0px';
			readonly thresholds = [0];
			constructor(callback: IntersectionObserverCallback) { intersectionCallback = callback; }
			disconnect() {}
			observe() {}
			takeRecords(): IntersectionObserverEntry[] { return []; }
			unobserve() {}
		});
		const mounted = vi.fn();
		const unmounted = vi.fn();
		const load = vi.fn(async () => 'loaded');
		const Component = defineComponent({
			props: ['data'],
			setup() {
				onMounted(mounted);
				onBeforeUnmount(unmounted);
			},
			template: '<div>owned: {{ data }}</div>',
		});
		await launchServerPlugins([plugin('example', lazyProvider({ requiresDetail: false, load, component: Component }))]);
		const result = renderPreview({ detail: false });

		intersectionCallback?.([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver);
		await waitFor(() => expect(result.getByText('owned: loaded')).toBeTruthy());
		expect(load).toHaveBeenCalledTimes(1);
		expect(mounted).toHaveBeenCalledTimes(1);

		intersectionCallback?.([{ isIntersecting: false } as IntersectionObserverEntry], {} as IntersectionObserver);
		await nextTick();
		intersectionCallback?.([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver);
		await nextTick();

		expect(result.getByText('owned: loaded')).toBeTruthy();
		expect(load).toHaveBeenCalledTimes(1);
		expect(mounted).toHaveBeenCalledTimes(1);
		expect(unmounted).not.toHaveBeenCalled();
	});

	test('keeps a ready provider mounted across document inactivity without reloading', async () => {
		const mounted = vi.fn();
		const unmounted = vi.fn();
		const load = vi.fn(async () => 'loaded');
		const Component = defineComponent({
			props: ['data', 'context'],
			setup() {
				onMounted(mounted);
				onBeforeUnmount(unmounted);
			},
			template: '<div>document: {{ data }}:{{ context.active }}</div>',
		});
		await launchServerPlugins([plugin('example', lazyProvider({ load, component: Component }))]);
		const result = renderPreview();

		await waitFor(() => expect(result.getByText('document: loaded:true')).toBeTruthy());
		Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
		document.dispatchEvent(new Event('visibilitychange'));
		await waitFor(() => expect(result.getByText('document: loaded:false')).toBeTruthy());

		Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
		document.dispatchEvent(new Event('visibilitychange'));
		await waitFor(() => expect(result.getByText('document: loaded:true')).toBeTruthy());
		expect(load).toHaveBeenCalledTimes(1);
		expect(mounted).toHaveBeenCalledTimes(1);
		expect(unmounted).not.toHaveBeenCalled();
	});

	test('renders a provider loading card only while one bounded load is in flight', async () => {
		let finish!: (value: string) => void;
		const Loading = defineComponent({ template: '<div aria-label="loading preview">loading card</div>' });
		await launchServerPlugins([plugin('example', lazyProvider({
			requiresDetail: false,
			loadingComponent: Loading,
			load: () => new Promise(resolve => { finish = resolve; }),
		}))]);

		const result = renderPreview({ detail: false });
		await waitFor(() => expect(result.getByLabelText('loading preview')).toBeTruthy());
		finish('loaded');
		await waitFor(() => expect(result.getByText('provider: loaded')).toBeTruthy());
		expect(result.queryByLabelText('loading preview')).toBeNull();
	});

	test('aborts a pending load when document visibility is lost and on unmount', async () => {
		const signals: AbortSignal[] = [];
		const load = vi.fn((_input, _context, signal) => {
			signals.push(signal);
			return new Promise(() => undefined);
		});
		await launchServerPlugins([plugin('example', lazyProvider({ load }))]);

		const first = renderPreview();
		await waitFor(() => expect(load).toHaveBeenCalledTimes(1));
		Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
		document.dispatchEvent(new Event('visibilitychange'));
		await waitFor(() => expect(signals[0].aborted).toBe(true));
		first.unmount();

		Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
		const second = renderPreview();
		await waitFor(() => expect(load).toHaveBeenCalledTimes(2));
		second.unmount();
		expect(signals[1].aborted).toBe(true);
	});

	test('aborts a pending load when a kept-alive preview is deactivated', async () => {
		let signal: AbortSignal | undefined;
		const load = vi.fn((_input, _context, value) => {
			signal = value;
			return new Promise(() => undefined);
		});
		await launchServerPlugins([plugin('example', lazyProvider({ load }))]);
		const shown = ref(true);
		const Wrapper = defineComponent({
			components: { MkPluginUrlPreview },
			setup: () => ({ shown, source }),
			template: '<KeepAlive><MkPluginUrlPreview v-if="shown" :url="url" :detail="true" :revealed="true" :source="source" /></KeepAlive>',
			data: () => ({ url: urlInput.href }),
		});

		render(Wrapper, { global: { directives, components } });
		await waitFor(() => expect(load).toHaveBeenCalledTimes(1));
		shown.value = false;
		await nextTick();

		expect(signal?.aborted).toBe(true);
	});

	test('keeps a ready provider across kept-alive deactivation without reloading', async () => {
		const mounted = vi.fn();
		const unmounted = vi.fn();
		const load = vi.fn(async () => 'loaded');
		const Component = defineComponent({
			props: ['data'],
			setup() {
				onMounted(mounted);
				onBeforeUnmount(unmounted);
			},
			template: '<div>kept alive: {{ data }}</div>',
		});
		await launchServerPlugins([plugin('example', lazyProvider({ load, component: Component }))]);
		const shown = ref(true);
		const Wrapper = defineComponent({
			components: { MkPluginUrlPreview },
			setup: () => ({ shown, source }),
			template: '<KeepAlive><MkPluginUrlPreview v-if="shown" :url="url" :detail="true" :revealed="true" :source="source" /></KeepAlive>',
			data: () => ({ url: urlInput.href }),
		});

		const result = render(Wrapper, { global: { directives, components } });
		await waitFor(() => expect(result.getByText('kept alive: loaded')).toBeTruthy());
		shown.value = false;
		await nextTick();
		shown.value = true;
		await waitFor(() => expect(result.getByText('kept alive: loaded')).toBeTruthy());

		expect(load).toHaveBeenCalledTimes(1);
		expect(mounted).toHaveBeenCalledTimes(1);
		expect(unmounted).not.toHaveBeenCalled();
	});

	test('closes a load failure to a source link without trying another provider', async () => {
		const secondLoad = vi.fn(async () => 'second');
		await launchServerPlugins([
			plugin('first', lazyProvider({
				priority: 10,
				async load() {
					throw new Error('load failed');
				},
			})),
			plugin('second', lazyProvider({ priority: 5, load: secondLoad })),
		]);

		const result = renderPreview();
		await waitFor(() => expect(result.getByRole('link').getAttribute('href')).toBe(urlInput.href));
		expect(secondLoad).not.toHaveBeenCalled();
		expect(fetchMock).not.toHaveBeenCalled();
	});

	test('keeps a load failure settled across viewport exit and re-entry', async () => {
		const setIntersecting = controlIntersection();
		const load = vi.fn(async () => {
			throw new Error('load failed');
		});
		await launchServerPlugins([plugin('broken', lazyProvider({ requiresDetail: false, load }))]);
		const result = renderPreview({ detail: false });

		setIntersecting(true);
		await waitFor(() => expect(load).toHaveBeenCalledTimes(1));
		await waitFor(() => expect(result.getByRole('link').getAttribute('href')).toBe(urlInput.href));
		setIntersecting(false);
		await nextTick();
		setIntersecting(true);
		await nextTick();

		expect(load).toHaveBeenCalledTimes(1);
		expect(fetchMock).not.toHaveBeenCalled();
	});

	test('retries a settled load failure when the ownership key changes', async () => {
		const load = vi.fn(async () => {
			throw new Error('load failed');
		});
		await launchServerPlugins([plugin('broken', lazyProvider({ load }))]);
		const result = renderPreview();

		await waitFor(() => expect(load).toHaveBeenCalledTimes(1));
		await result.rerender({ url: 'https://example.com/post/2' });
		await waitFor(() => expect(load).toHaveBeenCalledTimes(2));
	});

	test('retries a settled load failure after its CW closes and reopens', async () => {
		const load = vi.fn(async () => {
			throw new Error('load failed');
		});
		await launchServerPlugins([plugin('broken', lazyProvider({ load }))]);
		const result = renderPreview();

		await waitFor(() => expect(load).toHaveBeenCalledTimes(1));
		await result.rerender({ revealed: false });
		await result.rerender({ revealed: true });
		await waitFor(() => expect(load).toHaveBeenCalledTimes(2));
	});

	test('captures a provider render error and closes to a source link', async () => {
		vi.spyOn(console, 'error').mockImplementation(() => undefined);
		await launchServerPlugins([plugin('broken', lazyProvider({
			component: defineComponent({
				setup() {
					throw new Error('render failed');
				},
				template: '<div/>',
			}),
		}))]);

		const result = renderPreview();
		await waitFor(() => expect(result.getByRole('link').getAttribute('href')).toBe(urlInput.href));
		expect(fetchMock).not.toHaveBeenCalled();
	});

	test('keeps a provider render failure settled across viewport exit and re-entry', async () => {
		vi.spyOn(console, 'error').mockImplementation(() => undefined);
		const setIntersecting = controlIntersection();
		const load = vi.fn(async () => 'loaded');
		const renderAttempts = vi.fn();
		await launchServerPlugins([plugin('broken', lazyProvider({
			requiresDetail: false,
			load,
			component: defineComponent({
				setup() {
					renderAttempts();
					throw new Error('render failed');
				},
				template: '<div/>',
			}),
		}))]);
		const result = renderPreview({ detail: false });

		setIntersecting(true);
		await waitFor(() => expect(renderAttempts).toHaveBeenCalledTimes(1));
		await waitFor(() => expect(result.getByRole('link').getAttribute('href')).toBe(urlInput.href));
		setIntersecting(false);
		await nextTick();
		setIntersecting(true);
		await nextTick();

		expect(load).toHaveBeenCalledTimes(1);
		expect(renderAttempts).toHaveBeenCalledTimes(1);
		expect(fetchMock).not.toHaveBeenCalled();
	});

	test('captures a loading component render error, aborts the load, and closes to a source link', async () => {
		vi.spyOn(console, 'error').mockImplementation(() => undefined);
		let signal: AbortSignal | undefined;
		await launchServerPlugins([plugin('broken', lazyProvider({
			loadingComponent: defineComponent({
				setup() {
					throw new Error('loading render failed');
				},
				template: '<div/>',
			}),
			load(_input, _context, value) {
				signal = value;
				return new Promise(() => undefined);
			},
		}))]);

		const result = renderPreview();
		await waitFor(() => expect(result.getByRole('link').getAttribute('href')).toBe(urlInput.href));
		expect(signal?.aborted).toBe(true);
		expect(fetchMock).not.toHaveBeenCalled();
	});

	test('keeps a loading component failure settled across viewport exit and re-entry', async () => {
		vi.spyOn(console, 'error').mockImplementation(() => undefined);
		const setIntersecting = controlIntersection();
		const load = vi.fn(() => new Promise(() => undefined));
		const renderAttempts = vi.fn();
		await launchServerPlugins([plugin('broken', lazyProvider({
			requiresDetail: false,
			loadingComponent: defineComponent({
				setup() {
					renderAttempts();
					throw new Error('loading render failed');
				},
				template: '<div/>',
			}),
			load,
		}))]);
		const result = renderPreview({ detail: false });

		setIntersecting(true);
		await waitFor(() => expect(renderAttempts).toHaveBeenCalledTimes(1));
		await waitFor(() => expect(result.getByRole('link').getAttribute('href')).toBe(urlInput.href));
		setIntersecting(false);
		await nextTick();
		setIntersecting(true);
		await nextTick();

		expect(load).toHaveBeenCalledTimes(1);
		expect(renderAttempts).toHaveBeenCalledTimes(1);
		expect(fetchMock).not.toHaveBeenCalled();
	});
});
