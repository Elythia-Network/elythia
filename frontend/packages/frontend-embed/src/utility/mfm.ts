/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// MFM を扱う画面のコードの入口。解析器を読むのは frontend-shared の js/mfm.ts だけで、
// ここはそれを出し直す (#3515)。画面のコードは解析器を直接 import しない (eslint の
// no-restricted-imports で強制している)。

export * from '@@/js/mfm.js';
