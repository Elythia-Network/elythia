/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import * as mfm from '@/utility/mfm.js';

// mfm-js のパッチは途中の結果を覚えて使い直す。覚えるときの鍵 (入れ子の深さ、リンクの
// 見出しの中か) を取り違えると、入れ子の上限の近くで構文木が変わる。期待する木は、
// パッチをあてる前の mfm-js 0.26.0 の出力をそのまま写したもの。
const cases: { input: string; nestLimit: number; expected: unknown }[] = [
	{ input: "<center>**~~a~~**", nestLimit: 2, expected: [{"type":"text","props":{"text":"<center>"}},{"type":"bold","children":[{"type":"strike","children":[{"type":"text","props":{"text":"a"}}]}]}] },
	{ input: "\n<center>**~~a~~**\n", nestLimit: 2, expected: [{"type":"text","props":{"text":"\n<center>"}},{"type":"bold","children":[{"type":"strike","children":[{"type":"text","props":{"text":"a"}}]}]},{"type":"text","props":{"text":"\n"}}] },
	{ input: "**~~a~~**", nestLimit: 1, expected: [{"type":"bold","children":[{"type":"text","props":{"text":"~~a~~"}}]}] },
	{ input: "**~~a~~**", nestLimit: 2, expected: [{"type":"bold","children":[{"type":"strike","children":[{"type":"text","props":{"text":"a"}}]}]}] },
	{ input: "**~~a~~**", nestLimit: 3, expected: [{"type":"bold","children":[{"type":"strike","children":[{"type":"text","props":{"text":"a"}}]}]}] },
	{ input: "[**~~a~~**", nestLimit: 2, expected: [{"type":"text","props":{"text":"["}},{"type":"bold","children":[{"type":"strike","children":[{"type":"text","props":{"text":"a"}}]}]}] },
	{ input: "[**~~a~~**](https://a.example)", nestLimit: 2, expected: [{"type":"link","props":{"silent":false,"url":"https://a.example"},"children":[{"type":"bold","children":[{"type":"text","props":{"text":"~~a~~"}}]}]}] },
	{ input: "<center>**~~a~~**</center> **b**", nestLimit: 2, expected: [{"type":"text","props":{"text":"<center>"}},{"type":"bold","children":[{"type":"strike","children":[{"type":"text","props":{"text":"a"}}]}]},{"type":"text","props":{"text":"</center> "}},{"type":"bold","children":[{"type":"text","props":{"text":"b"}}]}] },
	{ input: "$[x2 **~~a~~**]", nestLimit: 2, expected: [{"type":"fn","props":{"name":"x2","args":{}},"children":[{"type":"bold","children":[{"type":"text","props":{"text":"~~a~~"}}]}]}] },
	{ input: "> **~~a~~**\n> **~~a~~**", nestLimit: 2, expected: [{"type":"quote","children":[{"type":"bold","children":[{"type":"text","props":{"text":"~~a~~"}}]},{"type":"text","props":{"text":"\n"}},{"type":"bold","children":[{"type":"text","props":{"text":"~~a~~"}}]}]}] },
];

describe('mfm parse near the nest limit', () => {
	test.each(cases)('parses $input with nestLimit $nestLimit as unpatched mfm-js does', ({ input, nestLimit, expected }) => {
		expect(mfm.parse(input, { nestLimit })).toEqual(expected);
	});
});
