/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /about-elythia が AGPL-3.0 section 13 の案内として成立していることを実ブラウザで
// 見る (#2700)。
//
// **`mkgo/` に置く。** `/about-elythia` は mk-go の frontend にしか無いので、公式 image
// を使う `playwright-ts-test` では必ず 404 になる。
//
// Go 側のテストは `EnsureInitial` が repositoryUrl を入れることと meta が
// providesTarball を false で返すことまでしか見ない。**新規インスタンスを起動
// した結果としてページに案内が出るか**は、DB 作成から通しでやらないと分からない。
// playwright スタックは clean DB から migration を流して起動する (`make
// playwright-check` / CI は `down -v` を挟む) ので、この spec が「新規インスタンスで
// 案内が出ない」状態への回帰を検出する経路になる。**volume を残したまま
// `playwright-up` だけを叩いた場合は既存 meta 行が残り、`migration/000084` の
// backfill が代わりに埋めるので `EnsureInitial` 側の回帰は捕まらない。**
//
// **検査していない導線が 2 つある。** サイドバーのインスタンスメニュー
// (`ui/_common_/common.ts` の `openInstanceMenu`) はボタンを押すまで DOM に無く、
// `MkSourceCodeAvailablePopup` は「サインイン済み + localStorage 未設定」でしか
// 出ない (`boot/main-boot.ts`)。どちらもこの spec の未認証セッションからは
// 到達できないので、回帰はレビューで見るしかない。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';

const MKGO_REPOSITORY_URL = 'https://github.com/shiroha-a/mk';
// #3379 で frontend を本体へ取り込む前に、frontend のソースとして案内していた fork。
const OLD_FRONTEND_REPOSITORY_URL = 'https://github.com/shiroha-a/misskey-ts';

test.describe('UI: /about-elythia', () => {
  test.setTimeout(60_000);

  // ページの前提。meta が空だとページ側はリンクを 1 本落とすだけなので、
  // 「案内が出た」ことの意味が変わる。先に固定する。
  test('a fresh instance advertises its source and ignores the tarball flag', async ({ request }) => {
    const resp = await callApi(request, 'meta', { detail: false });
    expect(resp.status()).toBe(200);
    const meta = await resp.json();

    expect(meta.repositoryUrl, 'repositoryUrl が空。EnsureInitial の既定値が効いていない').toBeTruthy();
    // **instance.yml は publishTarballInsteadOfProvideRepositoryUrl: true を
    // 明示している。** mk-go に /tarball/ のルートは無く、SPA catchall が HTML を
    // 200 で返してしまうので、設定が通ると壊れた tarball を配ることになる。
    // ここが true になったら `config.ProvidesTarball()` が設定値を返す形に
    // 戻っている。
    expect(meta.providesTarball, 'providesTarball が true。mk-go に /tarball/ は無い').toBe(false);
  });

  test('renders the Elythia page with source, license and Misskey attribution', async ({ page, baseURL }) => {
    const resp = await page.goto(`${baseURL}/about-elythia`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // **このサーバーが動かしているコードの案内。** repositoryUrl が未設定
    // (NULL / 空 / upstream の列 DEFAULT のまま) だとページ側が落とすので、
    // EnsureInitial と migration 000084 の回帰はここで落ちる。mk-go 本体への
    // リンクとは href が一致しうるため、`data-testid` で区別する。
    const serverSource = page.locator('[data-testid="about-elythia-server-source"] a');
    await expect(serverSource).toBeVisible({ timeout: 20_000 });
    await expect(serverSource).toHaveAttribute('href', /^https?:\/\//);

    // **mk-go 本体のソース。** #3379 で frontend を本体へ取り込んだので、いま表示されて
    // いる画面のソースもここで案内される。このサーバーの案内先と同じ URL なら
    // そちらとして、違えば mk-go 本体として、どちらかで必ず出る。アーカイブした
    // fork を「frontend のソース」として案内し続けていないことも見る。
    await expect(page.locator(`a[href="${MKGO_REPOSITORY_URL}"]`).first()).toBeVisible();
    await expect(page.locator(`a[href="${OLD_FRONTEND_REPOSITORY_URL}"]`)).toHaveCount(0);

    // ライセンスの提示。AGPL であることが分かる導線が消えたら落とす。
    await expect(page.locator(`a[href="${MKGO_REPOSITORY_URL}/blob/develop/LICENSE"]`)).toBeVisible();

    // Misskey への帰属。ページから /about-misskey へ辿れること。
    await expect(page.locator('a[href="/about-misskey"]')).toBeVisible();
  });

  // **tarball リンクを見るのは /about-misskey と /about。** `about-elythia.vue` は
  // tarball の分岐を持たないので、そこで数えても常に 0 で空振りする。この 2 つが
  // `instance.providesTarball` を読む唯一の場所 (fork 全体を grep して確認)。
  test('no tarball link is rendered even though the config enables it', async ({ page, baseURL }) => {
    await page.goto(`${baseURL}/about-misskey`, { waitUntil: 'domcontentloaded' });
    // ページの mount を待ってから数える (mount 前は何も無いので必ず 0 になる)。
    await expect(page.locator('a[href="/about-elythia"]')).toBeVisible({ timeout: 20_000 });
    await expect(page.locator('a[href^="/tarball/"]')).toHaveCount(0);

    await page.goto(`${baseURL}/about`, { waitUntil: 'domcontentloaded' });
    await expect(page.locator('a[href="/about-elythia"]').first()).toBeVisible({ timeout: 20_000 });
    await expect(page.locator('a[href^="/tarball/"]')).toHaveCount(0);
  });

  // エントランス右上の GitHub リボン。#2700 が導線 3 箇所を `/about-elythia` へ向けた
  // ときの取りこぼしで、#2890 で直した。**upstream が触るファイルの 1 行**なので、
  // 追従で元の外部リンク (Misskey 本体のリポジトリ) に戻されたらここで落ちる。
  //
  // `isRoot` は `mainRouter.currentRoute.value.name === 'index'` なので
  // **`/` でしか描画されない** (`/about-misskey` などでは出ない)。
  test('the entrance ribbon points at the mk-go source page', async ({ page, baseURL }) => {
    await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });

    // **`<a>` に `toBeVisible()` は使えない。** 中の `<svg>` が
    // `position: fixed` で親のレイアウトから外れるため、`<a>` 自身の
    // bounding box は 0 になり "hidden" と判定される (upstream から変わって
    // いない構造で、実際には svg が右上に描画されクリックも届く)。
    // href の一致で行き先を固定し、描画は svg 側で見る。
    const ribbon = page.locator('a.github-corner');
    await expect(ribbon).toHaveAttribute('href', '/about-elythia', { timeout: 20_000 });
    await expect(page.locator('a.github-corner svg')).toBeVisible();
  });

  test('navigates from /about-elythia to /about-misskey and back', async ({ page, baseURL }) => {
    await page.goto(`${baseURL}/about-elythia`, { waitUntil: 'domcontentloaded' });
    await page.locator('a[href="/about-misskey"]').first().click();
    await page.waitForURL('**/about-misskey', { timeout: 20_000 });

    // 逆向きの導線。upstream のページからも mk-go 側へ戻れること。
    await page.locator('a[href="/about-elythia"]').first().click();
    await page.waitForURL('**/about-elythia', { timeout: 20_000 });
  });
});
