/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, vi, beforeEach } from 'vitest';

// **実物の i18n / preferences / media-proxy は読み込まない。** locale 全体や
// meta タグを引き込むので、検証したい分岐ではなく環境の都合で落ちる。
vi.mock('@/i18n.js', () => ({
	i18n: {
		ts: {
			_emojiApplication: {
				previewFileGone: 'FILE_GONE',
				previewRemoteGone: 'REMOTE_GONE',
				previewApprovedEmojiGone: 'APPROVED_GONE',
				previewUnknown: 'UNKNOWN',
				previewLoadFailed: 'LOAD_FAILED',
			},
		},
	},
}));

const prefs = { disableShowingAnimatedImages: false };
vi.mock('@/preferences.js', () => ({
	prefer: { get s() { return prefs; } },
}));

vi.mock('@/utility/media-proxy.js', () => ({
	getProxiedImageUrl: (url: string, type?: string, mustOrigin?: boolean, noFallback?: boolean) =>
		`proxied(${url},${type},${mustOrigin},${noFallback})`,
	getStaticImageUrl: (url: string) => `static(${url})`,
}));

const { applicationPreviewUrl, applicationPreviewMessage } = await import('@/utility/emoji-request-preview.js');
type Preview = Parameters<typeof applicationPreviewUrl>[0];

const localFile: Preview = { url: 'https://self/f1.png', source: 'applicationFile', state: 'available' };
const remote: Preview = { url: 'https://other.example/e.webp', source: 'remoteEmoji', state: 'available' };
const approved: Preview = { url: 'https://self/e1.webp', source: 'approvedEmoji', state: 'available' };

describe('applicationPreviewUrl', () => {
	beforeEach(() => { prefs.disableShowingAnimatedImages = false; });

	// **リモートの生 URL は本番の CSP (img-src 'self') で読めない。**
	// #2903 / #2935 / #2957 で 3 回踏んだ形。
	test('リモート絵文字は media proxy を通す', () => {
		expect(applicationPreviewUrl(remote, false))
			.toBe('proxied(https://other.example/e.webp,emoji,false,true)');
	});

	// 自サーバーの URL は通さない (proxy は allowlist を持つので余計に失敗しうる)。
	test('申請元の drive ファイルはそのまま', () => {
		expect(applicationPreviewUrl(localFile, false)).toBe('https://self/f1.png');
	});

	test('承認後の絵文字はそのまま', () => {
		expect(applicationPreviewUrl(approved, false)).toBe('https://self/e1.webp');
	});

	// **静止画設定を尊重する。** `getProxiedImageUrl` の第 4 引数は `noFallback`
	// であって静止画とは無関係なので、包まないとここだけアニメーションが動く。
	test('静止画設定が on なら getStaticImageUrl で包む', () => {
		prefs.disableShowingAnimatedImages = true;
		expect(applicationPreviewUrl(localFile, false)).toBe('static(https://self/f1.png)');
		expect(applicationPreviewUrl(remote, false))
			.toBe('static(proxied(https://other.example/e.webp,emoji,false,true))');
	});

	test('解決できていないものは出さない', () => {
		expect(applicationPreviewUrl(undefined, false)).toBeNull();
		expect(applicationPreviewUrl({ ...localFile, state: 'sourceGone', url: '' }, false)).toBeNull();
		expect(applicationPreviewUrl({ ...localFile, state: 'unknown' }, false)).toBeNull();
		// state が available でも url が空なら出さない。
		expect(applicationPreviewUrl({ ...localFile, url: '' }, false)).toBeNull();
	});

	test('読み込みに失敗したものは出さない', () => {
		expect(applicationPreviewUrl(localFile, true)).toBeNull();
	});
});

describe('applicationPreviewMessage', () => {
	// **読み込み失敗を「削除済み」に丸めない。** API 上は存在しているので嘘になる。
	test('読み込み失敗が最優先', () => {
		expect(applicationPreviewMessage(localFile, true)).toBe('LOAD_FAILED');
		expect(applicationPreviewMessage({ ...localFile, state: 'sourceGone' }, true)).toBe('LOAD_FAILED');
	});

	test('申請元の削除は種別で文面が変わる', () => {
		expect(applicationPreviewMessage({ ...localFile, state: 'sourceGone', url: '' }, false)).toBe('FILE_GONE');
		expect(applicationPreviewMessage({ ...remote, state: 'sourceGone', url: '' }, false)).toBe('REMOTE_GONE');
	});

	test('承認後の削除は専用の文面', () => {
		expect(applicationPreviewMessage({ ...approved, state: 'approvedEmojiGone', url: '' }, false))
			.toBe('APPROVED_GONE');
	});

	// **DB 障害を「削除済み」に丸めない。**
	test('確認できなかった場合は専用の文面', () => {
		expect(applicationPreviewMessage({ ...localFile, state: 'unknown', url: '' }, false)).toBe('UNKNOWN');
		// preview 自体が無い (古い応答) も同じ扱い。
		expect(applicationPreviewMessage(undefined, false)).toBe('UNKNOWN');
	});
});
