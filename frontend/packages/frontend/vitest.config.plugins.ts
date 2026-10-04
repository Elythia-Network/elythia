/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vitest/config';
import unitConfig from './vitest.config.unit.js';

const require = createRequire(import.meta.url);
// mk-go の plugins/ を指す (frontend/packages/frontend から 3 段上、mk-go #3379)。
// 外れると include が空振りし、vitest は No test files found で落ちる。
const pluginRoot = fileURLToPath(new URL('../../../plugins', import.meta.url));
const pluginTestPattern = path.join(pluginRoot, '*/frontend/*.test.ts').replaceAll(path.sep, '/');

// ビルド時プラグインの回帰テストは、本体の通常テストとは別に実行する。
export default defineConfig({
	...unitConfig,
	server: {
		...unitConfig.server,
		fs: {
			...unitConfig.server?.fs,
			allow: [...(unitConfig.server?.fs?.allow ?? []), pluginRoot],
		},
	},
	resolve: {
		...unitConfig.resolve,
		alias: {
			...unitConfig.resolve?.alias,
			'@testing-library/vue': require.resolve('@testing-library/vue'),
		},
	},
	test: {
		...unitConfig.test,
		include: [pluginTestPattern],
	},
});
