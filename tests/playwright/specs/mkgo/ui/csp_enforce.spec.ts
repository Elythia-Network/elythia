/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */
import { expect, test } from '@playwright/test';

// SPA shell が CSP enforce 下で動くことを実ブラウザで確かめる (#2788)。
//
// **`mkgo/` に置く。** `frontendContentSecurityPolicy` は mk-go 独自キーで、
// 公式 image は無視する。`upstream/` に置くと `playwright-ts-test` で必ず落ちる。
//
// Go 側の `TestFrontendCSP_HashesCoverRenderedInlineScripts` は HTML と CSP
// header の突き合わせで、**ブラウザが実際に script を実行できるか**は見ない。
// inline event handler が hash では通らない (#2786 の High) のように、
// ブラウザに実行させないと分からない挙動がある。
test.describe('CSP enforce', () => {
	// **header の確認を先に置く。** これが無いと、`instance.yml` から
	// `frontendContentSecurityPolicy` が消えても violation ゼロで緑になり、
	// 「CSP を検査していないのに通る」状態に落ちる。下の spec が意味を持つ
	// 前提そのものなので、独立したテストとして固定する。
	test('SPA shell is served with an enforcing CSP header', async ({ page, baseURL }) => {
		const resp = await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });
		expect(resp).not.toBeNull();
		const headers = resp!.headers();

		// report-only では script が block されないので、enforce であることまで見る。
		expect(
			headers['content-security-policy-report-only'],
			'report-only が返っている。enforce でないと violation が出ても script は動く',
		).toBeUndefined();

		const csp = headers['content-security-policy'];
		expect(csp, 'CSP header が無い。instance.yml の frontendContentSecurityPolicy を確認').toBeTruthy();
		// #2786 で外した `'unsafe-inline'` が script 側に戻っていないこと。
		// directive は `;` 区切りなので、そこで切ってから script-src を見る
		// (`style-src 'unsafe-inline'` は意図的に残してあり、単純な部分一致だと
		// そちらを拾ってしまう)。
		// `startsWith('script-src')` にすると、将来 `script-src-attr` を足したとき
		// そちらを先に拾う。directive 名の完全一致で取る。
		const scriptSrc = csp
			.split(';')
			.map((d) => d.trim())
			.find((d) => d === 'script-src' || d.startsWith('script-src '));
		expect(scriptSrc, 'script-src directive が無い').toBeTruthy();
		expect(scriptSrc, "script-src に 'unsafe-inline' が戻っている").not.toContain("'unsafe-inline'");
		expect(scriptSrc, 'script-src に inline script の hash が無い').toContain("'sha256-");
	});

	test('SPA boots with no script-src violation', async ({ page, baseURL }) => {
		const violations: string[] = [];
		page.on('console', (msg) => {
			const t = msg.text();
			if (!t.includes('Content Security Policy')) return;
			// **script の実行系だけを見る** (`script-src` / その fallback の
			// `default-src` / `worker-src`)。resource 系 (`img-src` /
			// `connect-src` / `media-src` / `frame-src` / `style-src` /
			// `font-src`) は対象外で、`img-src` は実際に違反が出る
			// (`/about-misskey` の外部画像。docs/playwright.md)。
			if (/script-src|default-src|worker-src/.test(t)) violations.push(t);
		});

		await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });

		// **boot の完了を待ってから assert する。** violation はブロックされた
		// 時点で出るので、mount 前に数えると「まだ何も実行していないから 0」に
		// なる。smoke spec と同じ条件で Vue の mount を待つ。
		//
		// **timeout を catch する。** `waitForFunction` は時間切れで `TimeoutError`
		// を throw するので、そのままだと下の assertion に到達しない。SPA が
		// 一切 mount しないのは shell の inline script が全部 block された最悪の
		// ケースそのもので、**どの script が何の directive で落ちたか**という
		// 一番欲しい情報が、まさにそのときだけ失われる。
		//
		// per-test timeout (30s、playwright.config.ts) より短くしてあるのは、
		// テスト全体の時間切れが先に出ると catch する隙も無いため。
		let mounted = true;
		await page
			.waitForFunction(
				() => {
					const app = document.querySelector('#app, #misskey_app');
					if (app && app.children.length > 0) return true;
					return document.querySelector('main, nav, button[type], a[href]:not([href=""])') !== null;
				},
				null,
				{ timeout: 20000 },
			)
			.catch(() => {
				mounted = false;
			});

		// violation を先に出す。mount しなかった原因がこれなら、こちらのほうが
		// メッセージとして具体的。
		expect(violations, `script が CSP で block された: ${violations.join(' / ')}`).toHaveLength(0);
		expect(mounted, 'SPA が mount しなかった (CSP violation は無いので別の原因)').toBe(true);
	});
});
