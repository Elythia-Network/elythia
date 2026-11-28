/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { localeHash } from '../../../sw/locale-hash.js';
import { localeCacheName, localeFetchInit } from '../../../sw/src/scripts/locale-cache.js';
import { keepCacheOnActivate } from '../../../sw/src/scripts/maintenance.js';

// SW の lang.ts は読み込んだ時点で IndexedDB から言語を読む。happy-dom には無いので差し替える。
// テストから解決した id で差し替えるので、sw と frontend の idb-keyval の版が揃っている (同じ実体になる) 前提
vi.mock('idb-keyval', () => ({
	get: vi.fn(async () => 'ja-JP'),
	set: vi.fn(async () => undefined),
}));

const builtDir = join(__dirname, '../../../../built');
const rebuildHint = '先に frontend をビルドする (make e2e-frontend-build、または cd frontend && pnpm build)';

afterEach(() => {
	vi.unstubAllGlobals();
	vi.resetModules();
});

describe('SW locale cache', () => {
	it('names the cache after both the version and the locale content', () => {
		expect(localeCacheName('2026.11.1', 'aaaa')).not.toBe(localeCacheName('2026.11.1', 'bbbb'));
		expect(localeCacheName('2026.11.1', 'aaaa')).not.toBe(localeCacheName('2026.11.2', 'aaaa'));
		expect(keepCacheOnActivate(localeCacheName('2026.11.1', 'aaaa'), localeCacheName('2026.11.1', 'bbbb'))).toBe(false);
	});

	it('changes the hash when a single word of the locales changes', () => {
		const base = { 'ja-JP': { _lang_: '日本語', reject: '拒否' }, 'en-US': { _lang_: 'English', reject: 'Reject' } };
		const changed = { ...base, 'ja-JP': { ...base['ja-JP'], reject: '却下' } };
		const reordered = { 'en-US': base['en-US'], 'ja-JP': base['ja-JP'] };
		expect(localeHash(changed)).not.toBe(localeHash(base));
		expect(localeHash(reordered)).toBe(localeHash(base));
	});

	it('stores the locale in the hashed cache and revalidates it over HTTP', async () => {
		const hash = '0123456789abcdef';
		const opened: string[] = [];
		const put = vi.fn(async () => undefined);
		vi.stubGlobal('_LOCALE_HASH_', hash);
		vi.stubGlobal('caches', {
			match: vi.fn(async () => undefined),
			open: vi.fn(async (name: string) => {
				opened.push(name);
				return { put };
			}),
		});
		const fetchMock = vi.fn(async () => new Response(JSON.stringify({ _lang_: '日本語', reject: '拒否' })));
		vi.stubGlobal('fetch', fetchMock);

		// 型検査 (vue-tsc) の対象に sw のファイルを入れないよう、指定を変数にして読む。frontend の
		// tsconfig では sw の `@/` も `_LOCALE_HASH_` も解決できない (vitest は selfAlias で解決する)
		const langModule = join(__dirname, '../../../sw/src/scripts/lang.ts');
		const { swLang } = await import(/* @vite-ignore */ langModule) as {
			swLang: { cacheName: string; setLang: (lang: string) => Promise<{ ts: Record<string, string> }> };
		};
		const expectedName = `mk-cache-${_VERSION_}-${hash}`;
		expect(swLang.cacheName).toBe(expectedName);

		const i18n = await swLang.setLang('ja-JP');
		expect(i18n.ts.reject).toBe('拒否');
		expect(fetchMock).toHaveBeenCalledTimes(1);
		expect(fetchMock).toHaveBeenCalledWith(`/assets/locales/ja-JP.${_VERSION_}.json`, expect.objectContaining({ cache: 'no-cache' }));
		expect(localeFetchInit(new AbortController().signal).cache).toBe('no-cache');
		await vi.waitFor(() => expect(put).toHaveBeenCalled());
		expect(opened).toEqual([expectedName]);
	});
});

describe('built Service Worker', () => {
	// 配る locale (i18n の writeFrontendLocalesJson が書く JSON) から hash を求め直し、ビルドした
	// sw.js に埋め込まれた値と一致するかを見る。sw の build.js が別のものから hash を求めていれば
	// 落ちる。ビルドの成果物を読むので、先に frontend をビルドしておく (CI は pnpm build の後に流す)。
	// 手元で build.js の変異を確かめるときも、ビルドし直してから流す
	it('embeds the hash of the locales the server serves', () => {
		const swPath = join(builtDir, '_sw_dist_/sw.js');
		const localesDir = join(builtDir, '_frontend_dist_/locales');
		// skip にはしない (前提が欠けたのを成功に見せない)。ENOENT より先に作り方を出して落とす
		if (!existsSync(swPath) || !existsSync(localesDir)) {
			throw new Error(`frontend の成果物 (${swPath} と ${localesDir}) が無い。${rebuildHint}`);
		}
		const suffix = `.${_VERSION_}.json`;
		const files = readdirSync(localesDir).filter(f => f.endsWith(suffix));
		// i18n だけを作り直すと locale の JSON だけが新しくなり、sw.js は古い hash のまま残る
		const newestLocale = Math.max(...files.map(f => statSync(join(localesDir, f)).mtimeMs));
		if (statSync(swPath).mtimeMs < newestLocale) {
			throw new Error(`sw.js が locale の JSON より古い (i18n だけを作り直した?)。${rebuildHint}`);
		}
		const sw = readFileSync(swPath, 'utf8');
		const served: Record<string, unknown> = {};
		for (const file of files) {
			const locale = JSON.parse(readFileSync(join(localesDir, file), 'utf8')) as Record<string, unknown>;
			delete locale._version_;
			served[file.slice(0, -suffix.length)] = locale;
		}
		// ja-JP が無いのは、読む場所か名前の形を取り違えている
		expect(served['ja-JP']).toBeDefined();
		expect(sw, `sw.js の hash が配る locale と合わない。${rebuildHint}`).toContain(JSON.stringify(localeHash(served)));
	});
});
