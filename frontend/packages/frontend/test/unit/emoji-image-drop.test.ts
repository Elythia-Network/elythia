/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, test, expect, vi } from 'vitest';

// 実物の i18n は locale 全体と `@@/js/config.js` (meta タグを読む) を引き込む。
vi.mock('@/i18n.js', () => ({
	i18n: {
		ts: {
			_emojiApplication: {
				errorDropMultiple: 'MULTIPLE',
				errorDropUnsupported: 'UNSUPPORTED',
			},
		},
	},
}));
import { EMOJI_IMAGE_TYPES, isEmojiImageType, pickDroppedEmojiImage, droppedEmojiImageErrorText } from '@/utility/emoji-image-drop.js';

function fileOf(type: string, name = 'a'): File {
	return new File([new Uint8Array([1])], name, { type });
}

/**
 * mk-go: カスタム絵文字申請へのドラッグ＆ドロップ (#2959)。
 *
 * **判定をコンポーネントの外に置いてある。** `.vue` のイベントハンドラに
 * 埋めると単体テストから駆動できず、件数と形式の分岐が無検証になる。
 */
describe('pickDroppedEmojiImage', () => {
	test('画像 1 件はそのまま採る', () => {
		const f = fileOf('image/png');
		expect(pickDroppedEmojiImage([f])).toEqual({ ok: true, file: f });
	});

	// **黙って 1 件目を採らない。** どれが申請されたのか分からないまま名前や
	// ライセンスを書くことになる。
	test('複数はエラーにする', () => {
		const r = pickDroppedEmojiImage([fileOf('image/png', 'a'), fileOf('image/gif', 'b')]);
		expect(r).toEqual({ ok: false, reason: 'multiple' });
	});

	test('対応していない形式はエラーにする', () => {
		expect(pickDroppedEmojiImage([fileOf('image/svg+xml')])).toEqual({ ok: false, reason: 'unsupported' });
		expect(pickDroppedEmojiImage([fileOf('application/pdf')])).toEqual({ ok: false, reason: 'unsupported' });
		expect(pickDroppedEmojiImage([fileOf('text/plain')])).toEqual({ ok: false, reason: 'unsupported' });
	});

	// ファイル以外 (テキスト選択など) のドロップ。エラーを出すと、意図しない
	// ドロップのたびにダイアログが出る。
	test('ファイルが無ければ黙って無視する', () => {
		expect(pickDroppedEmojiImage([])).toEqual({ ok: false, reason: 'none' });
	});
});

describe('isEmojiImageType', () => {
	// **リテラルで書く。** `test.each(EMOJI_IMAGE_TYPES)` だと被検査対象を
	// 自分で回すので、**エントリが消えても件数が減るだけ**で緑のまま通る
	// (実測で 22 → 21 tests)。backend との一致は Go 側のゲートが見るが、
	// こちらでも集合そのものを固定しておく。
	const expected = [
		'image/png', 'image/gif', 'image/jpeg', 'image/webp', 'image/avif',
		'image/apng', 'image/bmp', 'image/tiff', 'image/x-icon',
	];
	test('allowlist が変わっていない', () => {
		expect([...EMOJI_IMAGE_TYPES].sort()).toEqual([...expected].sort());
	});
	test.each(expected)('%s を受け入れる', (mime) => {
		expect(isEmojiImageType(mime)).toBe(true);
	});

	// **ブラウザが返す別名を拒否しない。** MIME は環境依存で、サーバーは中身
	// から判定し直す (upstream misskey#16091)。ここで弾くとサーバーなら通る
	// 画像を拒否することになる。
	test.each(['image/jpg', 'image/vnd.microsoft.icon', 'image/ico'])('別名 %s も受け入れる', (mime) => {
		expect(isEmojiImageType(mime)).toBe(true);
	});

	// **空文字列は通す。** ブラウザが種類を判定できなかっただけで、画像でない
	// とは限らない。最終判定はサーバーが行う。
	test('空文字列は通す', () => {
		expect(isEmojiImageType('')).toBe(true);
	});

	test.each(['image/svg+xml', 'application/pdf', 'text/plain', 'video/mp4', 'image'])('%s は拒否する', (mime) => {
		expect(isEmojiImageType(mime)).toBe(false);
	});
});

/**
 * mk-go: 拒否の理由と文面の対応 (#2959)。
 *
 * **三項演算子を `.vue` に書くと取り違えても何も落ちない。** 「複数落としたのに
 * 形式が悪いと言われる」ような案内になる。
 */
describe('droppedEmojiImageErrorText', () => {
	test('理由ごとに別の文面を返す', () => {
		expect(droppedEmojiImageErrorText('multiple')).toBe('MULTIPLE');
		expect(droppedEmojiImageErrorText('unsupported')).toBe('UNSUPPORTED');
	});
});
