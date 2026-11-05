/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import * as Elythia from 'elythia-js';

// elythia-js の `export * from 'misskey-js'` が、型だけでなく値も再公開していることを確かめる。
// package.json の exports の default (実行時の入口) は、ここでは確かめられない。vitest は
// 入口が無くても解決するため。入口が要るのは、src のコードが elythia-js から値を import した
// ときの vite の解決 (開発サーバーと本番ビルド)。#4028 の時点では src に値の import は無い
describe('elythia-js re-exports the runtime values of misskey-js (#4028)', () => {
	test('acct, Stream, and the constants are usable from elythia-js', () => {
		expect(Elythia.acct.parse('@alice@remote.example')).toEqual({ username: 'alice', host: 'remote.example' });
		expect(Elythia.acct.toString({ username: 'alice', host: null })).toBe('alice');
		expect(typeof Elythia.Stream).toBe('function');
		expect(Elythia.noteVisibilities).toEqual(['public', 'home', 'followers', 'specified']);
	});
});
