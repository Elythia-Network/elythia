/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import { cachedRemoteFileCount, cleanRemoteFilesState } from '@/utility/remote-cache-cleanup.js';
import type { RemoteUsageBucket } from '@/utility/remote-cache-cleanup.js';

function bucket(over: Partial<RemoteUsageBucket> = {}): RemoteUsageBucket {
	return { count: 0, size: 0, linkCount: 0, ...over };
}

describe('cleanRemoteFilesState', () => {
	test('集計が無ければ「対象が無い」と断定しない', () => {
		expect(cleanRemoteFilesState(null)).toBe('unknown');
		expect(cleanRemoteFilesState(undefined)).toBe('unknown');
	});

	// mk-go 生まれの本番はこの形 (実測: remote 74,922 件すべて isLink、size 0)。
	test('すべて link なら対象なし', () => {
		expect(cleanRemoteFilesState(bucket({ count: 74922, linkCount: 74922 }))).toBe('none');
	});

	test('リモート行が 1 件も無くても対象なし', () => {
		expect(cleanRemoteFilesState(bucket())).toBe('none');
	});

	// 純正から引き継いだ DB。これが押せないのが #3102。
	test('実体つきの行が 1 件でもあれば押せる', () => {
		expect(cleanRemoteFilesState(bucket({ count: 100, linkCount: 99, size: 1234 }))).toBe('ready');
	});

	// **`size > 0` で数えない。** 純正が length を取れなかった実体つきの行は
	// size が 0 のまま残るので、size で判定すると取りこぼす。
	test('size が 0 でも実体つきなら押せる', () => {
		expect(cleanRemoteFilesState(bucket({ count: 10, linkCount: 3, size: 0 }))).toBe('ready');
	});

	// 集計が別々のクエリ由来なので、順序次第で linkCount が count を上回りうる。
	test('linkCount が count を上回っても負にしない', () => {
		expect(cleanRemoteFilesState(bucket({ count: 5, linkCount: 9 }))).toBe('none');
		expect(cachedRemoteFileCount(bucket({ count: 5, linkCount: 9 }))).toBe(0);
	});
});

describe('cachedRemoteFileCount', () => {
	test.each([
		[{ count: 0, linkCount: 0 }, 0],
		[{ count: 10, linkCount: 10 }, 0],
		[{ count: 10, linkCount: 4 }, 6],
		[{ count: 10, linkCount: 0 }, 10],
	])('%o -> %i', (over, want) => {
		expect(cachedRemoteFileCount(bucket(over))).toBe(want);
	});
});
