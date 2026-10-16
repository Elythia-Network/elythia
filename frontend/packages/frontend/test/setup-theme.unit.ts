/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 本番では起動処理 (`boot/common.ts`) が描画より先に必ずテーマを当てるので、
// 部品は `themeManager.currentCompiledTheme!` を null でない前提で読む
// (`directives/panel.ts`、`utility/init-chart.ts` など)。unit test には起動処理が
// 無く、テーマ未設定のまま部品を描くと非同期の読み込みや `mounted` の中で
// `Cannot read properties of null` になり、vitest の unhandled error (exit 1) として
// 不定期に落ちる (#3366、#3369)。部品の側に null の分岐を足すと読む箇所ごとに
// 直すことになるので、テストの側で本番と同じ前提 (テーマが当たっている) を作る。
//
// **`@/theme.js` が読み込まれたときにだけ当てる (`vi.doMock` の factory の中)。**
// setup で `@/theme.js` を自分から import すると、テーマを使わないテストにまで
// `@/os.js` などの依存を読み込ませることになる。実測で、`@/i.js` や
// `@@/js/config.js` を部分的に mock しているテストなど 5 ファイルが読み込みの途中で
// 落ち、`drop-and-fusion.test.ts` では使わない API (`i/registry/get-all`) を叩いて
// unhandled rejection になった。
//
// **`@/theme.js` そのものを試すファイルには当てない** (`setup-theme.exclusions.ts`)。
// mock した module は mock のレジストリに残るので、当てたファイルでは
// `vi.resetModules()` しても `@/theme.js` は作り直されず、ファイルの中で同じ
// `ThemeManager` を共有し続ける (実測)。本家の `theme.test.ts` は毎回新しい
// インスタンスを得る前提なので外す。除外を条件付きにするため `vi.mock` (巻き上げ
// られる) でなく `vi.doMock` で書く。
//
// 当てたファイルでの既知の性質:
// - 読み込んだ時点で `localStorage` に `theme` / `themeId` / `themeCachedVersion` が
//   書かれ、`<html>` の style に `--MI_THEME-*` と `data-color-scheme` が付く
// - `updateTheme` は同じ id で `themeCachedVersion` が一致すると何もしないので、
//   テストが同じテーマ (Elythia Light) を `updateTheme` してもイベントは出ない
// - `@/theme.js` の読み込みの時点で `updateTheme` が走るので、その依存
//   (`@@/js/config.js` の `version`、`@/local-storage.js` など) を部分的に mock して
//   中身を欠かしたテストは、使う前でも import で落ちる (下で原因を添えて投げ直す)
// - テストファイルが自分で `vi.mock('@/theme.js')` / `vi.doMock` すればそちらが優先
//   される。`vi.doUnmock('@/theme.js')` するとこの setup の mock ごと外れ、以後の
//   読み込みはテーマ未設定になる

import { expect, vi } from 'vitest';
import { THEME_SETUP_EXCLUDED_FILES } from './setup-theme.exclusions.js';

const testPath = expect.getState().testPath;
// 取れないと除外が黙って効かなくなるので、vitest の挙動が変わったら落とす。
if (typeof testPath !== 'string' || testPath === '') {
	throw new Error('setup-theme.unit.ts: expect.getState().testPath is not available; cannot decide whether to apply the test theme');
}
const normalizedPath = testPath.replaceAll('\\', '/');
const excluded = THEME_SETUP_EXCLUDED_FILES.some((name) => normalizedPath.endsWith(`/test/unit/${name}`));

if (!excluded) {
	vi.doMock('@/theme.js', async (importOriginal) => {
		const mod = await importOriginal<typeof import('@/theme.js')>();
		const { default: elythiaLight } = await import('@@/themes/l-elythia.json5');
		try {
			mod.themeManager.updateTheme(elythiaLight);
		} catch (err) {
			throw new Error('setup-theme.unit.ts: failed to apply the test theme while loading @/theme.js (a partial vi.mock of its dependencies may be missing exports)', { cause: err });
		}
		return mod;
	});
}
