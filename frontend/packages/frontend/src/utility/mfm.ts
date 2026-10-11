/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// MFM を扱う唯一の入口。画面のコードは mfm-js を直接 import せず、ここを使う
// (eslint の no-restricted-imports で強制している)。関数の名前・引数・戻り値は
// mfm-js と同じにしてあり、後で Elythia の解析器へ中身を差し替えられるようにしている。

import * as mfmjs from 'mfm-js';

export type {
	MfmBlock,
	MfmBold,
	MfmCenter,
	MfmCodeBlock,
	MfmEmojiCode,
	MfmFn,
	MfmHashtag,
	MfmInline,
	MfmInlineCode,
	MfmItalic,
	MfmLink,
	MfmMathBlock,
	MfmMathInline,
	MfmMention,
	MfmNode,
	MfmPlain,
	MfmQuote,
	MfmSearch,
	MfmSimpleNode,
	MfmSmall,
	MfmStrike,
	MfmText,
	MfmUnicodeEmoji,
	MfmUrl,
	NodeType,
} from 'mfm-js';

/**
 * Parses MFM like mfm-js's `parse`.
 */
export function parse(input: string, opts?: Partial<{ nestLimit: number }>): mfmjs.MfmNode[] {
	// mfm-js の公開の引数だけを渡す (パッチのテスト用の口は通さない)。
	return mfmjs.parse(input, { nestLimit: opts?.nestLimit });
}

/**
 * Parses the simple MFM syntax like mfm-js's `parseSimple`.
 */
// parseSimple は絵文字と平文しか読まないので、長い入力でも重くならない (3000 文字の
// どの形でも 10ms 前後だった)。そのまま渡す。
export function parseSimple(input: string): mfmjs.MfmSimpleNode[] {
	return mfmjs.parseSimple(input);
}

/**
 * Collects the nodes for which `predicate` returns true, like mfm-js's
 * `extract`.
 */
export function extract(nodes: mfmjs.MfmNode[], predicate: (node: mfmjs.MfmNode) => boolean): mfmjs.MfmNode[] {
	return mfmjs.extract(nodes, predicate);
}
