/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Elythia: テーマのコード (JSON5) の解析を theme.ts から分けた (#3613)。theme.ts は起動時に
// 必ず読まれるので、そこに json5 を置くと、テーマを入れる操作のためだけのライブラリ
// (gzip で約 10KB) を全員が起動時に読むことになる。

import JSON5 from 'json5';
import { validateTheme } from './theme.js';
import type { Theme } from './theme.js';

export function parseThemeCode(code: string): Theme {
	let theme;

	try {
		theme = JSON5.parse(code);
	} catch (_) {
		throw new Error('Failed to parse theme json');
	}
	if (!validateTheme(theme)) {
		throw new Error('This theme is invaild');
	}

	return theme;
}
