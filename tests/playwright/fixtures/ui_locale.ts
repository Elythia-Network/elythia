/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 英語の UI に日本語の文言が混ざっていないかを見る spec 共通の helper (#4047)。
// ロケールは ja-JP を土台に重ねるので、en-US に無い語や、テンプレートに直接書いた
// 日本語は、英語の UI でもそのまま出る。

/**
 * Matches hiragana, katakana and CJK ideographs. The browser locale of the specs is
 * en-US (playwright.config.ts), so any match on a page means a string fell back to ja-JP.
 */
export const JAPANESE = /[぀-ヿ㐀-䶿一-鿿]/;
