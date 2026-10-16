/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// `@/theme.js` を読み終えた直後 (このモジュールの評価中) に、テーマが当たって
// いたかを同期で記録する (`test/unit/setup-theme.test.ts` が使う)。テストファイルの
// 最上位で記録すると、後ろに並ぶ import の読み込みの間にタイマーが着地するので、
// テーマを当てるのを遅らせた変更を拾えない (実測)。

import { themeManager } from '@/theme.js';

/** Whether a compiled theme was current right after `@/theme.js` finished loading. */
export const themedAtImport = themeManager.currentCompiledTheme != null;
