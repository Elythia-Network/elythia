/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { afterEach, assert, describe, test, vi } from 'vitest';

// instance.ts はimportした時点でHTMLのmetaと手元のキャッシュを比べるので、
// 場面ごとにDOMとlocalStorageを整えてから読み直す
async function loadInstanceModule(opts: { providedAt?: number; cachedAt?: number }) {
	window.document.getElementById('misskey_meta')?.remove();
	window.localStorage.removeItem('instance');
	window.localStorage.removeItem('instanceCachedAt');
	if (opts.providedAt != null) {
		const el = window.document.createElement('script');
		el.type = 'application/json';
		el.id = 'misskey_meta';
		el.dataset.generatedAt = String(opts.providedAt);
		el.textContent = JSON.stringify({ name: 'provided' });
		window.document.head.appendChild(el);
	}
	if (opts.cachedAt != null) {
		window.localStorage.setItem('instance', JSON.stringify({ name: 'cached' }));
		window.localStorage.setItem('instanceCachedAt', String(opts.cachedAt));
	}
	vi.resetModules();
	return await import('@/instance.js');
}

describe('consumeProvidedMeta', () => {
	afterEach(() => {
		window.document.getElementById('misskey_meta')?.remove();
		window.localStorage.removeItem('instance');
		window.localStorage.removeItem('instanceCachedAt');
		vi.resetModules();
	});

	test('reports the embedded meta only once', async () => {
		const mod = await loadInstanceModule({ providedAt: 2000, cachedAt: 1000 });
		assert.strictEqual(mod.instance.name, 'provided');
		assert.isTrue(mod.consumeProvidedMeta());
		assert.isFalse(mod.consumeProvidedMeta());
	});

	test('does not report when the local cache was newer than the embedded meta', async () => {
		const mod = await loadInstanceModule({ providedAt: 1000, cachedAt: 2000 });
		assert.strictEqual(mod.instance.name, 'cached');
		assert.isFalse(mod.consumeProvidedMeta());
	});

	test('does not report when no meta is embedded', async () => {
		const mod = await loadInstanceModule({ cachedAt: 2000 });
		assert.isFalse(mod.consumeProvidedMeta());
	});
});
