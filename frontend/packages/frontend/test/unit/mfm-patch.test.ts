/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { createRequire } from 'node:module';
import { describe, expect, test } from 'vitest';
import * as mfmjs from 'mfm-js';

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
		expect(source).toContain('workLimit: opts.workLimit != null ? opts.workLimit : WORK_LIMIT_BASE + WORK_LIMIT_PER_CHAR * input.length');
	});
});

// 1 回の解析の仕事量に上限 (安全網) を置いてある。時間には頼らず、テストのための口
// (workLimit) で上限を小さくして、上限を超えた後の読み方を確かめる。
describe('mfm-js work limit', () => {
	type ParseWithLimit = (input: string, opts: { nestLimit?: number; workLimit?: number }) => mfmjs.MfmNode[];
	const parse = mfmjs.parse as unknown as ParseWithLimit;

	test('reads the rest as plain text once the work limit is exceeded', () => {
		expect(parse('**a** :b:', { workLimit: 0 })).toEqual([{ type: 'text', props: { text: '**a** :b:' } }]);
	});

	test('parses as before while the work stays under the limit', () => {
		expect(parse('**a** :b:', { workLimit: 1_000_000 })).toEqual(mfmjs.parse('**a** :b:'));
		expect(mfmjs.parse('**a** :b:')[0].type).toBe('bold');
	});

	test('switches to plain text partway through a long input', () => {
		const input = '**a** '.repeat(200);
		const nodes = parse(input, { workLimit: 2000 });
		expect(nodes[0].type).toBe('bold');
		expect(nodes.at(-1)).toEqual({ type: 'text', props: { text: expect.stringContaining('**a**') } });
	});

	test('keeps long ordinary posts under the default limit', () => {
		const post = [
			'**太字**と<i>斜体</i>と~~打ち消し~~、`code`、\\(x^2\\)、$[x2 大きく] ',
			'リンク [ここ](https://example.com/a_(b)) と <https://example.com/x> ',
			'@user@remote.example #タグ #(かっこ) :neko: https://example.com/path?q=[1] ',
			'JSON: {"a": [1, 2, {"b": [3]}], "c": "[x]"} ',
			'> 引用\n> **太字**\n',
			'気温<20度、i < n ',
		].join('').repeat(60).slice(0, 8192);
		expect(parse(post, {})).toEqual(parse(post, { workLimit: Number.POSITIVE_INFINITY }));
	});
});
