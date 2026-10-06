/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */
import { expect, test } from '@playwright/test';

// boot エラー画面の Reload ボタンが押せること (#2786)。
//
// **`mkgo/` に置く。** `#mkBootReload` は fork の `2026.7.0-mk.22c` で足した id で、
// upstream 2026.7.0 の boot.js には無い (`onclick` 属性のまま)。`upstream/` に
// 置くと `playwright-ts-test` が公式 image に対して回したときに必ず落ちる。
//
// **inline event handler は CSP の hash では通らない。** `script-src` に
// `'sha256-...'` を登録しても `onclick="..."` は block される
// (`'unsafe-hashes'` が要る)。mk-go は #2786 で `script-src` から
// `'unsafe-inline'` を外したので、`boot.js` の Reload ボタンを
// `addEventListener` に移した (fork の custom commit)。
//
// **押せないと「Failed to initialize Elythia」画面から復帰できない。** この画面が
// 出るのは CLIENT_ENTRY チャンクの 404 や boot 中の例外で、唯一の復旧手段が
// このボタン。到達するのは boot 完了前に限られる (`boot/common.ts` が
// `app.mount()` 後に `window.onerror = null` する) が、それはこの画面が出る条件
// そのもの。
//
// **主たるゲートはセレクタ側。** `onclick` 属性に戻すと `#mkBootReload` という
// id ごと消えるので、下の violation の assertion に届く前に 15s タイムアウトで
// 落ちる (実測)。upstream の boot.js は id を持たず `onclick` だけなので、
// 「id を残して `onclick` に戻す」は現実には起きない形。
//
// violation の assertion は**別の inline handler が混入したとき**に反応する。
// pass する run では空配列同士の比較のままだが、CSP が off だった頃と違って
// **原理的に落ちうる** (#2788 で enforce にした)。この spec が守っている主体は
// `#mkBootReload` が押せることそのもの。
test('boot error screen reload button is clickable', async ({ page }) => {
	const violations: string[] = [];
	page.on('console', (msg) => {
		const t = msg.text();
		if (t.includes('Content Security Policy')) violations.push(t);
	});

	// CLIENT_ENTRY を 404 にして boot を失敗させ、エラー画面を出す。
	await page.route('**/vite/**', (route) => route.fulfill({ status: 404, body: '' }));
	await page.goto('/');

	const btn = page.locator('#mkBootReload');
	await expect(btn).toBeVisible({ timeout: 15000 });

	// 押すとリロードが走る (= handler が登録されている)。
	const nav = page.waitForNavigation({ timeout: 10000 });
	await btn.click();
	await nav;

	// inline handler が CSP で block されていないこと (#2788 で enforce にした)。
	const handlerViolations = violations.filter((v) => v.includes('event handler'));
	expect(handlerViolations, `inline event handler が block された: ${handlerViolations.join(' / ')}`).toHaveLength(0);
});
