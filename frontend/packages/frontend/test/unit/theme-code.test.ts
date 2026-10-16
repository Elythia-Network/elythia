/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parseThemeCode } from '@@/js/theme-code.js';

// テーマのコードの解析を theme.ts から分けた (#3613)。
describe('parseThemeCode', () => {
	it('parses JSON5 theme code', () => {
		const theme = parseThemeCode(`{
			// comment
			id: 'x', name: 'X', author: 'a', base: 'dark',
			props: { accent: '#fff', },
		}`);
		expect(theme).toEqual({ id: 'x', name: 'X', author: 'a', base: 'dark', props: { accent: '#fff' } });
	});

	it('rejects code that is not JSON5', () => {
		expect(() => parseThemeCode('{')).toThrow('Failed to parse theme json');
	});

	it('rejects an invalid theme', () => {
		expect(() => parseThemeCode('{ id: "x", name: "X", base: "sepia", props: {} }')).toThrow('This theme is invaild');
	});
});

// theme.ts は起動時に必ず読まれる。json5 を import し直すと、起動時の JS に戻る。
describe('frontend-shared theme.ts', () => {
	it('does not import json5', () => {
		const source = readFileSync(join(__dirname, '../../../frontend-shared/js/theme.ts'), 'utf8');
		expect(source).not.toMatch(/from\s+'json5'/);
	});
});
