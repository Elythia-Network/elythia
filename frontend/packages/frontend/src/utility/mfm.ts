/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// MFM を扱う唯一の入口。画面のコードは解析器を直接 import せず、ここを使う
// (eslint の no-restricted-imports で強制している)。中身は Elythia の解析器
// @elythia-network/mfm (#3515)。関数の名前・引数・戻り値・構文木は mfm-js 0.26.0 と同じ。

import * as mfmParser from '@elythia-network/mfm';

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
} from '@elythia-network/mfm';

/**
 * Parses MFM like mfm-js's `parse`.
 */
export function parse(input: string, opts?: Partial<{ nestLimit: number }>): mfmParser.MfmNode[] {
	// 引数は mfm-js と同じ nestLimit だけを渡す。500 より大きい値は 500 として扱われる
	// (入れ子を再帰で読むため)。画面のコードは既定の 20 しか使わない。
	return mfmParser.parse(input, { nestLimit: opts?.nestLimit });
}

/**
 * Parses the simple MFM syntax like mfm-js's `parseSimple`.
 */
export function parseSimple(input: string): mfmParser.MfmSimpleNode[] {
	return mfmParser.parseSimple(input);
}

/**
 * Collects the nodes for which `predicate` returns true, like mfm-js's
 * `extract`.
 */
export function extract(nodes: mfmParser.MfmNode[], predicate: (node: mfmParser.MfmNode) => boolean): mfmParser.MfmNode[] {
	return mfmParser.extract(nodes, predicate);
}
