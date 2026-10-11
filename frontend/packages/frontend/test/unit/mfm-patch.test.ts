/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { createRequire } from 'node:module';
import { describe, expect, test } from 'vitest';

// mfm-js には pnpm の patchedDependencies でパッチをあてている
// (frontend/patches/mfm-js@0.26.0.patch)。install の手順が変わってパッチが
// 外れても、解析の結果は変わらないので他のテストでは気付けない。入った
// mfm-js のファイルにパッチの中身があることを確かめる。
describe('mfm-js patch', () => {
	const require = createRequire(import.meta.url);
	const built = dirname(require.resolve('mfm-js'));

	test.each(['index.mjs', 'index.cjs'])('is applied to %s', (file) => {
		const source = readFileSync(join(built, file), 'utf8');
		expect(source).toContain('function memoTable(state, input, id)');
		expect(source).toContain('parser.manyEnd = (input, index, state) => {');
		expect(source).toContain('memo: new Map()');
	});
});
