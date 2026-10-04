/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { i18n } from '@/i18n.js';

/**
 * MIME types the server accepts as a custom emoji image (#2959).
 *
 * **backend の `allowedImageTypes` と同じ集合にする。** ズレると、手前で
 * 受け入れたのに申請で `UNSUPPORTED_FILE_TYPE` になる (drive に無駄な
 * ファイルが残る) か、サーバーが受け入れる画像を手前で拒否することになる。
 * 一致は `TestEmojiApplicationIsWired` が検査する。
 */
export const EMOJI_IMAGE_TYPES = [
	'image/png',
	'image/gif',
	'image/jpeg',
	'image/webp',
	'image/avif',
	'image/apng',
	'image/bmp',
	'image/tiff',
	'image/x-icon',
];

/**
 * Aliases browsers report for types the server does accept.
 *
 * **こちらは frontend にだけ置く。** ブラウザが返す MIME は環境依存で、
 * サーバーは中身から判定し直す (upstream misskey#16091 が「こっち側で検出する
 * MIME type とサーバーで検出する MIME type は異なる場合がある」として
 * drive のアップロードでは手前の判定そのものを止めている)。ここで別名を
 * 拒否すると、**サーバーなら通る画像を弾く**ことになる。
 */
const EMOJI_IMAGE_TYPE_ALIASES = [
	'image/jpg',
	'image/vnd.microsoft.icon',
	'image/ico',
];

/**
 * Reports whether a dropped file may back a custom emoji.
 *
 * **空文字列は通す。** ブラウザが種類を判定できなかっただけで、画像でないとは
 * 限らない。最終的な判定はサーバーが行う。
 */
export function isEmojiImageType(mime: string): boolean {
	if (mime === '') return true;
	return EMOJI_IMAGE_TYPES.includes(mime) || EMOJI_IMAGE_TYPE_ALIASES.includes(mime);
}

export type DroppedEmojiImage =
	| { ok: true; file: File }
	| { ok: false; reason: 'none' | 'multiple' | 'unsupported' };

/**
 * Validates what was dropped onto the application form (#2959).
 *
 * **判定をコンポーネントの外に置く。** `.vue` のイベントハンドラに埋めると
 * 単体テストから駆動できず、件数と形式の分岐が無検証になる。
 */
export function pickDroppedEmojiImage(files: readonly File[]): DroppedEmojiImage {
	if (files.length === 0) return { ok: false, reason: 'none' };
	// **複数は黙って 1 件目を採らない。** どれが申請されたのか分からないまま
	// 名前やライセンスを書くことになる。
	if (files.length > 1) return { ok: false, reason: 'multiple' };
	const file = files[0];
	if (!isEmojiImageType(file.type)) return { ok: false, reason: 'unsupported' };
	return { ok: true, file };
}

/**
 * Maps a rejection reason onto the message shown to the user (#2959).
 *
 * **`.vue` の三項演算子に書かない。** 対応を取り違えても型もテストも通り、
 * 「複数落としたのに形式が悪いと言われる」ような案内になる。
 */
export function droppedEmojiImageErrorText(reason: 'multiple' | 'unsupported'): string {
	return reason === 'multiple'
		? i18n.ts._emojiApplication.errorDropMultiple
		: i18n.ts._emojiApplication.errorDropUnsupported;
}
