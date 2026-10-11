/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, assert, describe, expect, test, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor, type RenderResult } from '@testing-library/vue';
import { defineComponent, ref } from 'vue';
import XFederationHealth from '@/pages/admin/federation.health.vue';

const misskeyApi = vi.hoisted(() => vi.fn());
const confirm = vi.hoisted(() => vi.fn());
const apiWithDialog = vi.hoisted(() => vi.fn());

vi.mock('@/i18n.js', () => ({
	i18n: {
		ts: {
			all: 'All',
			host: 'Host',
			loadMore: 'Load more',
			sort: 'Sort',
			state: 'State',
			_federationHealth: {
				_hints: { serverError: 'Server error' },
				breakerClose: 'Resume delivery',
				breakerDraining: 'Draining',
				breakerNextProbe: 'Next probe',
				breakerOpen: 'Open',
				breakerOpenedAt: 'Opened at',
				breakerReservedUntil: 'Reserved until',
				breakersNote: 'Breaker note',
				breakersTitle: 'Breakers',
				breakerThrottled: 'Throttled',
				breakerThrottledUntil: 'Throttled until',
				degraded: 'Degraded',
				deliverNote: 'Deliver note',
				failed: 'Failed',
				hosts: 'Hosts',
				hostsCaption: 'Hosts caption',
				inboxNote: 'Inbox note',
				last1h: 'Last hour',
				last5m: 'Last 5 minutes',
				last15m: 'Last 15 minutes',
				noFailures: 'No failures',
				noHosts: 'No hosts',
				noRecordedError: 'No recorded error',
				noResponse: 'No response',
				outsideWindow: 'Outside window',
				overflow: 'Overflow',
				sortFailure: 'Failures',
				sortHost: 'Host',
				sortP95: 'P95',
				sortRate: 'Rate',
				sortVolume: 'Volume',
				stateDegraded: 'Degraded',
				stateFailing: 'Failing',
				stateHealthy: 'Healthy',
				succeeded: 'Succeeded',
				successRate: 'Success rate',
				unavailable: 'Unavailable',
				window: 'Window',
			},
		},
		tsx: {
			_federationHealth: {
				breakerCloseConfirm: ({ host }: { host: string }) => `Resume delivery to ${host}?`,
				breakerDetail: ({ failures, interval }: { failures: string; interval: string }) => `${failures} failures, ${interval}`,
				evicted: ({ n }: { n: string }) => `${n} evicted`,
				hours: ({ n }: { n: string }) => `${n} hours`,
				minutes: ({ n }: { n: string }) => `${n} minutes`,
			},
		},
	},
}));
vi.mock('@/components/MkFolder.vue', async () => {
	const { ref } = await import('vue');
	return { default: {
		props: { defaultOpen: { type: Boolean, default: false } },
		setup(props: { defaultOpen: boolean }) {
			return { open: ref(props.defaultOpen) };
		},
		template: `
			<section data-testid="health-folder" :data-default-open="String(defaultOpen)">
				<button data-testid="folder-toggle" type="button" @click="open = !open"><slot name="label"></slot><slot name="suffix"></slot></button>
				<div v-if="open"><slot></slot></div>
			</section>
		`,
	} };
});
vi.mock('@/components/MkButton.vue', () => ({
	default: {
		props: { disabled: { type: Boolean, default: false } },
		emits: ['click'],
		template: '<button type="button" :disabled="disabled" @click="$emit(\'click\')"><slot></slot></button>',
	},
}));
vi.mock('@/components/MkInput.vue', () => ({
	default: {
		props: { modelValue: { type: String, default: '' } },
		emits: ['update:modelValue'],
		template: '<label><slot name="label"></slot><input :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)"><slot name="prefix"></slot></label>',
	},
}));
vi.mock('@/components/MkSelect.vue', () => ({
	default: {
		props: {
			modelValue: { type: [String, Number], default: '' },
			items: { type: Array, default: () => [] },
		},
		emits: ['update:modelValue'],
		template: `
			<label>
				<slot name="label"></slot>
				<select :value="modelValue" @change="$emit('update:modelValue', $event.target.value)">
					<option v-for="item in items" :key="item.value" :value="item.value">{{ item.label }}</option>
				</select>
			</label>
		`,
	},
}));
vi.mock('@/components/MkInfo.vue', () => ({ default: { template: '<div><slot></slot></div>' } }));
vi.mock('@/components/global/MkLoading.vue', () => ({ default: { template: '<div><slot></slot></div>' } }));
vi.mock('@/components/global/MkTime.vue', () => ({ default: { template: '<div><slot></slot></div>' } }));
vi.mock('@/components/form/split.vue', () => ({ default: { template: '<div><slot></slot></div>' } }));
vi.mock('@/utility/misskey-api.js', () => ({ misskeyApi }));
vi.mock('@/os.js', () => ({ confirm, apiWithDialog }));

const MkFolderStub = defineComponent({
	props: {
		defaultOpen: {
			type: Boolean,
			default: false,
		},
	},
	setup(props) {
		return { open: ref(props.defaultOpen) };
	},
	template: `
		<section data-testid="health-folder" :data-default-open="String(defaultOpen)">
			<button data-testid="folder-toggle" type="button" @click="open = !open"><slot name="label"></slot><slot name="suffix"></slot></button>
			<div v-if="open"><slot></slot></div>
		</section>
	`,
});

const MkButtonStub = defineComponent({
	props: {
		disabled: {
			type: Boolean,
			default: false,
		},
	},
	emits: ['click'],
	template: '<button type="button" :disabled="disabled" @click="$emit(\'click\')"><slot></slot></button>',
});

const MkInputStub = defineComponent({
	props: {
		modelValue: {
			type: String,
			default: '',
		},
	},
	emits: ['update:modelValue'],
	template: '<label><slot name="label"></slot><input :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)"><slot name="prefix"></slot></label>',
});

const MkSelectStub = defineComponent({
	props: {
		modelValue: {
			type: [String, Number],
			default: '',
		},
		items: {
			type: Array,
			default: () => [],
		},
	},
	emits: ['update:modelValue'],
	template: `
		<label>
			<slot name="label"></slot>
			<select :value="modelValue" @change="$emit('update:modelValue', $event.target.value)">
				<option v-for="item in items" :key="item.value" :value="item.value">{{ item.label }}</option>
			</select>
		</label>
	`,
});

const EmptyStub = defineComponent({
	template: '<div><slot></slot><slot name="label"></slot><slot name="prefix"></slot></div>',
});

const global = {
	stubs: {
		MkFolder: MkFolderStub,
		MkButton: MkButtonStub,
		MkInput: MkInputStub,
		MkSelect: MkSelectStub,
		MkInfo: EmptyStub,
		MkLoading: EmptyStub,
		MkTime: EmptyStub,
		FormSplit: EmptyStub,
	},
};

function host(index: number) {
	return {
		host: `host-${String(index).padStart(4, '0')}.example.test`,
		success: index % 3 === 0 ? 1000 : 0,
		failure: index + 1,
		byClass: { serverError: index + 1 },
		latencyP50Ms: 100 + index,
		latencyP95Ms: 500 + index,
	};
}

function breaker(index: number) {
	return {
		host: `breaker-${String(index).padStart(4, '0')}.example.test`,
		open: true,
		consecutiveFailures: 5,
		openedAt: null,
		nextProbeAt: null,
		probeIntervalSeconds: 60,
		throttledUntil: null,
		reservedUntil: null,
	};
}

function healthResponse(hostCount = 2048, breakerCount = 256) {
	return {
		windowSeconds: 3600,
		hosts: Array.from({ length: hostCount }, (_, index) => host(index)),
		evictedHosts: 0,
		breakers: Array.from({ length: breakerCount }, (_, index) => breaker(index)),
	};
}

function hostFolders(view: RenderResult) {
	return view.queryAllByTestId('health-folder').filter((item) => /host-\d+\.example\.test/.test(item.textContent ?? ''));
}

function sortSelect(view: RenderResult): HTMLSelectElement {
	const select = view.getAllByRole('combobox').find((item) => item.querySelector('option[value="host"]'));
	assert.instanceOf(select, HTMLSelectElement);
	return select;
}

function stateSelect(view: RenderResult): HTMLSelectElement {
	const select = view.getAllByRole('combobox').find((item) => item.querySelector('option[value="failing"]'));
	assert.instanceOf(select, HTMLSelectElement);
	return select;
}

async function renderLargeDeliver() {
	misskeyApi.mockResolvedValue(healthResponse());
	const view = render(XFederationHealth, {
		props: { direction: 'deliver' },
		global,
	});
	await waitFor(() => assert.lengthOf(hostFolders(view), 50));
	return view;
}

afterEach(() => {
	cleanup();
	misskeyApi.mockReset();
	confirm.mockReset();
	apiWithDialog.mockReset();
});

describe('federation health large lists', () => {
	test.each(['deliver', 'inbox'] as const)('renders %s hosts in severity-aware, incremental pages', async (direction) => {
		misskeyApi.mockResolvedValue(healthResponse(120, direction === 'deliver' ? 25 : 0));

		const view = render(XFederationHealth, {
			props: { direction },
			global,
		});

		await waitFor(() => assert.lengthOf(hostFolders(view), 50));
		assert.ok(hostFolders(view).every((item) => item.dataset.defaultOpen === 'true'));

		await fireEvent.click(view.getAllByRole('button', { name: /load more/i }).at(-1)!);
		await waitFor(() => assert.lengthOf(hostFolders(view), 100));

		await fireEvent.click(view.getAllByRole('button', { name: /load more/i }).at(-1)!);
		await waitFor(() => assert.lengthOf(hostFolders(view), 120));
	});

	test('starts delivery breakers open and reveals ten at a time', async () => {
		const view = await renderLargeDeliver();
		const breakerFolder = view.getAllByTestId('health-folder')[0];

		assert.strictEqual(breakerFolder.dataset.defaultOpen, 'true');
		assert.lengthOf(view.container.querySelectorAll('.ti-player-play'), 10);

		await fireEvent.click(view.getAllByRole('button', { name: /load more/i })[0]);
		await waitFor(() => assert.lengthOf(view.container.querySelectorAll('.ti-player-play'), 20));
	});

	test('resets the host page after searching', async () => {
		const view = await renderLargeDeliver();

		await fireEvent.click(view.getAllByRole('button', { name: /load more/i }).at(-1)!);
		await waitFor(() => assert.lengthOf(hostFolders(view), 100));

		await fireEvent.update(view.getByRole('textbox'), 'example.test');
		await waitFor(() => assert.lengthOf(hostFolders(view), 50));
	});

	test('resets the host page and applies host sorting', async () => {
		const view = await renderLargeDeliver();

		await fireEvent.click(view.getAllByRole('button', { name: /load more/i }).at(-1)!);
		await waitFor(() => assert.lengthOf(hostFolders(view), 100));
		assert.include(hostFolders(view)[0].textContent, 'host-2047.example.test');
		assert.strictEqual(hostFolders(view)[0].dataset.defaultOpen, 'true');

		await fireEvent.update(sortSelect(view), 'host');
		await waitFor(() => assert.lengthOf(hostFolders(view), 50));
		assert.include(hostFolders(view)[0].textContent, 'host-0000.example.test');
		assert.strictEqual(hostFolders(view)[0].dataset.defaultOpen, 'false');
	});

	test('remounts a host folder when its severity becomes error', async () => {
		misskeyApi.mockImplementation(async (endpoint: string) => ({
			...healthResponse(0, 0),
			hosts: [{
				...host(0),
				...(endpoint.endsWith('inbox-health') ? { success: 0, failure: 10 } : {}),
			}],
		}));
		const view = render(XFederationHealth, {
			props: { direction: 'deliver' },
			global,
		});

		await waitFor(() => assert.lengthOf(hostFolders(view), 1));
		assert.strictEqual(hostFolders(view)[0].dataset.defaultOpen, 'false');
		assert.notInclude(hostFolders(view)[0].textContent, 'Succeeded');

		await view.rerender({ direction: 'inbox' });
		await waitFor(() => expect(misskeyApi).toHaveBeenLastCalledWith('admin/federation/inbox-health', { windowSeconds: 3600 }));
		await waitFor(() => assert.include(hostFolders(view)[0].textContent, 'Succeeded'));
		assert.strictEqual(hostFolders(view)[0].dataset.defaultOpen, 'true');
	});

	test('resets the host page after changing the state filter', async () => {
		const view = await renderLargeDeliver();

		await fireEvent.click(view.getAllByRole('button', { name: /load more/i }).at(-1)!);
		await waitFor(() => assert.lengthOf(hostFolders(view), 100));

		await fireEvent.update(stateSelect(view), 'failing');
		await waitFor(() => assert.lengthOf(hostFolders(view), 50));
	});

	test('resets pagination and refetches when changing from Deliver to Inbox', async () => {
		misskeyApi.mockImplementation(async (endpoint: string) => endpoint.endsWith('delivery-health')
			? healthResponse(120, 25)
			: healthResponse(80, 0));
		const view = render(XFederationHealth, {
			props: { direction: 'deliver' },
			global,
		});

		await waitFor(() => assert.lengthOf(hostFolders(view), 50));
		await fireEvent.click(view.getAllByRole('button', { name: /load more/i }).at(-1)!);
		await waitFor(() => assert.lengthOf(hostFolders(view), 100));

		await view.rerender({ direction: 'inbox' });
		await waitFor(() => expect(misskeyApi).toHaveBeenLastCalledWith('admin/federation/inbox-health', { windowSeconds: 3600 }));
		await waitFor(() => assert.lengthOf(hostFolders(view), 50));
	});

	test('keeps the delivery restart action working', async () => {
		confirm.mockResolvedValue({ canceled: false });
		apiWithDialog.mockResolvedValue(undefined);
		const view = await renderLargeDeliver();

		await fireEvent.click(view.getAllByRole('button', { name: /load more/i })[0]);
		await waitFor(() => assert.lengthOf(view.container.querySelectorAll('.ti-player-play'), 20));
		await fireEvent.click(view.getAllByRole('button', { name: /load more/i }).at(-1)!);
		await waitFor(() => assert.lengthOf(hostFolders(view), 100));

		const restartButton = view.container.querySelector('.ti-player-play')?.closest('button');
		assert.instanceOf(restartButton, HTMLButtonElement);
		await fireEvent.click(restartButton);

		await waitFor(() => expect(confirm).toHaveBeenCalledWith({
			type: 'question',
			text: expect.stringMatching(/breaker-0000\.example\.test/),
		}));
		await waitFor(() => expect(apiWithDialog).toHaveBeenCalledWith('admin/federation/close-delivery-breaker', {
			host: 'breaker-0000.example.test',
		}));
		await waitFor(() => assert.strictEqual(misskeyApi.mock.calls.length, 2));
		await waitFor(() => assert.lengthOf(hostFolders(view), 50));
		await waitFor(() => assert.lengthOf(view.container.querySelectorAll('.ti-player-play'), 10));
	});
});
